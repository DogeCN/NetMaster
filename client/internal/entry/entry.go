// Package entry 负责入口候选列表的来源。
//
// 入口 = 客户端拨的 Cloudflare 边缘地址。两个来源，合并去重后交给节点池探测：
//
//   - 服务端域名自身的 DNS 解析 —— 永远可用的兜底源，CF anycast 的 A/AAAA
//     记录本身就是合法入口；
//   - 社区优选 IP 源（内置多个，并行拉取）—— 精选过的低丢包入口，单个源
//     挂掉不影响其他，全挂掉退回到上次的磁盘缓存。
//
// 这里只决定"谁进候选集"；"先试谁"由 nodepool 探测打分决定 —— 快慢是
// "你的网络到那个 IP"的属性，只有客户端测得准。
package entry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"netmaster/internal/cache"
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
// 并行、逐源容错、全挂退缓存。列表顺序无关：最终顺序由本机探测决定。
var Sources = []string{
	"https://ipdb.api.030101.xyz/?type=bestcf&country=true",
	"https://addressesapi.090227.xyz/CloudFlareYes",
	"https://090227.pages.dev/bestcf?isp=all&ips=20",
}

// fetchTimeout 是单个源的超时。源的响应是一小段文本，超过这个时间基本就是死了。
const fetchTimeout = 4 * time.Second

// Community 并行拉取全部社区源，合并去重。
//
// 每次调用都会尝试网络（"每次启动更新"），成功的写入磁盘缓存；全部失败时
// 退回上次缓存（任何年龄的旧列表也比空列表强），没有缓存就返回空。
// 返回的 source 说明这次候选从哪来：net / cache / none。
func Community(ctx context.Context) (nodes []Node, source string) {
	type result struct {
		nodes []Node
		err   error
	}
	ch := make(chan result, len(Sources))
	var wg sync.WaitGroup
	for _, src := range Sources {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
			defer cancel()
			ns, err := fetchSource(fetchCtx, src)
			ch <- result{ns, err}
		}(src)
	}
	wg.Wait()
	close(ch)

	seen := make(map[string]struct{})
	for r := range ch {
		for _, n := range r.nodes {
			key := net.JoinHostPort(n.Addr, strconv.Itoa(int(n.Port)))
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			nodes = append(nodes, n)
		}
	}
	if len(nodes) > 0 {
		_ = cache.Save("entry", "community", nodes)
		return nodes, "net"
	}
	var cached []Node
	if _, ok := cache.Load("entry", "community", &cached); ok && len(cached) > 0 {
		return cached, "cache"
	}
	return nil, "none"
}

// fetchSource 拉取并宽容解析单个源。
func fetchSource(ctx context.Context, rawURL string) ([]Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "netmaster")
	resp, err := http.DefaultClient.Do(req)
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
	return parseList(string(body)), nil
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
		if len(out) >= 24 {
			break // 候选集够大了，再多只是浪费探测预算
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

// InvalidateCache 丢弃社区源缓存（供 --refresh 或排查时使用）。
func InvalidateCache() error {
	return cache.Clear("entry", "community")
}
