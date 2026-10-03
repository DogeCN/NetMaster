// Package entry 负责入口候选列表的来源。
//
// 入口 = 客户端拨的 Cloudflare 边缘地址。两个来源，合并去重后交给节点池探测：
//
//   - 服务端域名自身的 DNS 解析 —— 永远可用的兜底源，CF anycast 的 A/AAAA
//     记录本身就是合法入口；
//   - 社区优选 IP 源（内置多个，并行拉取）—— 精选过的低丢包入口，单个源
//     挂掉不影响其他；全部挂掉时只剩服务端域名解析兜底（不落盘缓存，
//     见 Community 的说明）。
//
// 这里只决定"谁进候选集"；"先试谁"由 selector 的延迟探测决定 —— 快慢是
// "你的网络到那个 IP"的属性，只有客户端测得准。
package entry

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"netmaster/internal/tlsfrag"
)

// Node 一个入口候选。SNI/鉴权不属于单个节点：SNI 是服务端域名（全局唯一），
// 鉴权由 PASSWORD 派生（全局唯一），都由调用方注入。
type Node struct {
	Addr string `json:"addr"`
	Port uint16 `json:"port"`
	Name string `json:"name,omitempty"`
}

// DefaultPort 入口的默认端口。
const DefaultPort = 443

// FromServer 解析服务端域名，把结果作为入口候选。
// 这是唯一不依赖任何第三方的源 —— 只要服务端域名能解析，客户端就有候选可用。
func FromServer(ctx context.Context, server string) []Node {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", server)
	if err != nil {
		return nil
	}
	out := make([]Node, 0, len(ips))
	seen := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		key := ip.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Node{Addr: key, Port: DefaultPort, Name: server})
	}
	return out
}

// Sources 内置社区优选源。全部是个人维护的公益服务，说死就死 —— 所以多源
// 并行、逐源容错、全挂退服务端域名解析。列表顺序无关：最终顺序由本机探测决定。
var Sources = []string{
	"https://ipdb.api.030101.xyz/?type=bestcf&country=true",
	"https://addressesapi.090227.xyz/CloudFlareYes",
	"https://090227.pages.dev/bestcf?isp=all&ips=50",
}

// fetchTimeout 是单个源的超时。源的响应是一小段文本，超过这个时间基本就是死了。
const fetchTimeout = 4 * time.Second

// maxPerSource 是单个源最多贡献的候选数。优选源给的就是排好序的头部结果，
// 尾部的重复/劣质条目没有探测价值。
const maxPerSource = 50

// Community 并行拉取全部社区源，合并去重。
//
// 每次调用都会尝试网络（"每次启动更新"），**不落盘**：不写磁盘缓存、失败也不读缓存。
// 理由是这几个源本身就是"IP 优选"性质的短名单 —— 缓存一份旧的没有价值，反而会让
// "为什么连不上"这类问题多出一个可能：拿到一份早已失效的旧列表还以为源挂了。
// 拉不到就如实返回空，让上层用服务端域名解析兜底（见 main.resolveEntries）。
//
// 拉取走 TLS 分片（见 fetchSource）：这几个源全是被墙域名，普通 TLS 握手送出的明文
// SNI 会让连接在 ClientHello 之后就被 RST —— 实测这三个源在同一台机器上，普通握手能
// 通但延迟高且不稳，分片之后才是可依赖的路径。
//
// 返回的 source 说明这次候选从哪来：net / none。
// CommunityLists 并发拉取全部社区源，返回**每个源各自的列表**（按 Sources 的固定
// 顺序，与网络到达顺序无关），并顺手剔掉不在 Cloudflare 网段内的条目。
//
// 为什么要"按源返回"而不是拍平：拍平只能按 channel 到达顺序合并，而三个源的响应
// 时间差异很大（协助者实测 522ms / 896ms / 1395ms），于是"合并后的前 N 条"每次
// 启动都不一样 —— 而 pages.dev 单源就返回 150 条 > maxEntries 64，截断前的顺序
// 直接决定哪 64 条活下来。**结果就是节点池不可复现，任何 A/B 测量都失去前提。**
// 按源返回后调用方能做按源交错取样：名额在源之间稳定分配，源挂掉时自动让位。
func CommunityLists(ctx context.Context) (lists [][]Node, source string) {
	type result struct {
		idx   int
		nodes []Node
	}
	ch := make(chan result, len(Sources))
	var wg sync.WaitGroup
	for i, src := range Sources {
		wg.Add(1)
		go func(i int, src string) {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
			defer cancel()
			ns, _ := fetchSource(fetchCtx, src)
			ch <- result{i, ns}
		}(i, src)
	}
	wg.Wait()
	close(ch)

	lists = make([][]Node, len(Sources))
	for r := range ch {
		lists[r.idx] = FilterCloudflare(r.nodes)
	}
	// 全灭就全灭：不落盘、不退缓存（entry 侧已取消缓存——订阅源是被墙域名，
	// 每次都靠分片 TLS 直连拉，缓存一份反而会掩盖"源挂了"这件事）。
	return lists, sourcesOf(lists)
}

// sourcesOf 把"哪些源真的贡献了候选"压成一行，启动日志会印给用户。
//
// 为什么单独抽出来：这一行曾经被硬编码成 "none"，于是"社区源全挂了"和
// "源一切正常"在用户眼里长得一模一样 —— 而这恰恰是排障时要看的第一个信息。
// 抽成纯函数是为了能离线钉住它（见 TestSourcesOf）。
func sourcesOf(lists [][]Node) string {
	names := make([]string, 0, len(lists))
	for i, l := range lists {
		if len(l) == 0 || i >= len(Sources) {
			continue
		}
		names = append(names, sourceLabel(Sources[i]))
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

// sourceLabel 把源 URL 压成日志里能一眼看懂的名字（域名）。
func sourceLabel(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}

// Community 拍平版：给诊断命令与只关心"合并结果"的调用点用。内部走 CommunityLists，
// 所以同样带网段过滤。
func Community(ctx context.Context) (nodes []Node, source string) {
	lists, src := CommunityLists(ctx)
	return Merge(lists...), src
}

// fragClient 是拉取订阅用的 HTTP 客户端：TLS 握手之后（也就是连接建立之后），
// 把前若干字节的**写**分片发出。
//
// 为什么需要：这三个源全是被墙域名。ClientHello 里的明文 SNI 会让连接在发出之后被
// RST，而 crypto/tls 自己写 ClientHello，我们碰不到那些字节 —— 只能在底层 TCP 连接上
// 包一层 tlsfrag.Conn。这也是为什么共享包要同时提供 Write（代理路径：整段 ClientHello
// 已在手）与 Conn（本路径：字节由 TLS 栈写出）两个入口。
//
// 只影响**写**方向：阻断发生在客户端发出的方向，读方向不需要动。
var fragClient = &http.Client{
	Timeout: fetchTimeout,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   false, // 自定义了 DialTLSContext，不再尝试 h2 升级
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: fetchTimeout,
		DialContext: (&net.Dialer{
			Timeout:   fetchTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// TLS 仍由标准库完成（证书照常校验，没有设 InsecureSkipVerify），
		// 只在它之下把写打散。
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: fetchTimeout}}
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return tlsfrag.NewConn(c), nil
		},
	},
}

func fetchSource(ctx context.Context, rawURL string) ([]Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "netmaster")
	resp, err := fragClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil && len(body) == 0 {
		return nil, err
	}
	// 剔掉不在 CF 官方网段内的条目：入口必须是承载我们域名的 CF 边缘地址，
	// 而订阅源是第三方维护的"优选 IP"列表，实测会混进非 CF 地址
	//（`090227.pages.dev` 150 条里 28 条）。留着的话它们先占 maxEntries 名额、
	// 再各花一次真实 TLS 探测预算，最后还是连不上。判据与实测见 cfnet.go。
	return FilterCloudflare(parseList(string(body))), nil
}

// entryRe 宽容匹配一行里的候选：IPv4 或主机名，可选 :端口，可带 #名字 后缀。
// 这些源有的返回纯文本，有的夹在 HTML 里，逐行剥出有效部分即可。
var entryRe = regexp.MustCompile(`((?:\d{1,3}(?:\.\d{1,3}){3})|(?:[a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)+))(?::(\d{1,5}))?(?:#([\w-]+))?`)

// parseList 从文本中剥出候选 IP/主机名。
func parseList(body string) []Node {
	var out []Node
	seen := make(map[string]struct{})
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
			continue
		}
		m := entryRe.FindStringSubmatch(strings.ToLower(line))
		if m == nil {
			continue
		}
		host, name := m[1], m[3]
		port := uint16(DefaultPort)
		if m[2] != "" {
			p, err := strconv.ParseUint(m[2], 10, 16)
			if err != nil {
				continue
			}
			port = uint16(p)
		}
		key := net.JoinHostPort(host, strconv.Itoa(int(port)))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Node{Addr: host, Port: port, Name: name})
		if len(out) >= maxPerSource {
			break
		}
	}
	return out
}

// Interleave 按源轮转交错取样，总数不超过 limit。
//
// 这是 B1 的修复：原先是"把各源拍平后取前 N 条"，而拍平顺序取决于三个源的响应
// 到达时间（实测 522ms / 896ms / 1395ms），且单源条目数可以超过上限本身
// （pages.dev 150 条 > 64）—— 于是**每次启动活下来的那批条目都不同**，节点池不可
// 复现，A/B 测量没有前提。
//
// 轮转的收益：
//
//	· 名额在源之间按比例稳定分配，池子可复现；
//	· 某个源挂掉或变短，它的名额自动让给其它源，不会留下空缺；
//	· 排在前面的仍然先是 DNS 源（调用方把它放在 lists[0]）。
func Interleave(lists [][]Node, limit int) []Node {
	if limit <= 0 || len(lists) == 0 {
		return nil
	}
	out := make([]Node, 0, limit)
	pos := make([]int, len(lists)) // 只读推进，绝不改调用方的切片
	for len(out) < limit {
		progressed := false
		for i, list := range lists {
			if len(out) >= limit {
				break
			}
			if pos[i] >= len(list) {
				continue // 这个源已取完，名额自然让给其它源
			}
			out = append(out, list[pos[i]])
			pos[i]++
			progressed = true
		}
		if !progressed {
			break // 所有源都取完了
		}
	}
	return out
}

// Merge 合并多个候选列表，按 addr:port 去重，保持传入顺序（先到者优先）。
func Merge(lists ...[]Node) []Node {
	var out []Node
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, n := range list {
			key := net.JoinHostPort(n.Addr, strconv.Itoa(int(n.Port)))
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}
