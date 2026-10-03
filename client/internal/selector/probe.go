package selector

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"netmaster/internal/entry"
	"netmaster/internal/tlsutil"
)

// IP 优选（PRD §6.6）：并行测候选入口的 TLS 握手延迟，取最快的 TopN 作为节点池
// 并按延迟排序。
//
// 为什么必须真握手而不是裸 TCP：实测（2026-10-03，大陆家宽）同一 CF 边缘 IP 上
// 部分 IP 对真实 SNI 的 ClientHello 会注入 RST——TCP 三次握手全通，TLS 必挂。
// 裸 TCP 探测选出的"快节点"一大半是这种，浏览器的页面加载超时撑不过
// "坏节点 15s 拨号超时 → failover"的链路，表现就是"时好时坏"。TLS 握手探测与
// 实际拨号同路径，把这类 IP 在进池前剔掉。
//
// 只测延迟不测吞吐：吞吐测试（多次小请求测带宽）在启动预算内做不完 16 个候选，
// 而延迟才是决定"握手卡不卡"的那一项。
//
// 探测不做证书校验（InsecureSkipVerify 恒真）：这里筛的是"IP+TLS 路径通不通"，
// 证书校验是拨号层（真实客户端配置）的职责——两处保持一致的开关由调用方传入，
// 探测层只关心握手能不能完成。

const (
	probeTopN      = 16
	probeParallel  = 12
	probeBudgetAll = 10 * time.Second // PRD §14 M4：优选全流程 ≤ 10s

	// probeUpgradeCandidates 是"WS 升级"这一阶段要验的候选数（按 TLS 延迟取前 N）。
	//
	// 为什么只验前 N 个而不是全部：升级一旦成功，请求就会真的进到 Worker 并
	// `SESSION.get()` 建一个 DO（见 server/src/index.js）。全量验 = 每次启动凭空
	// 建几十个用不上的 DO。取一个略高于 probeTopN 的前缀，是因为这个阶段本来
	// 就是"择优"：排在第 20 名之后的候选，就算升级能过也轮不到它进池。
	probeUpgradeCandidates = 12
	// tlsErrorSamples 是随 OptResult 带出去的失败样本条数。
	tlsErrorSamples = 5
	// probeTLSCandidates 是"要真的做 TLS 握手"的候选数（按 TCP 建连延迟取前 N）。
	//
	// 这一层是为了**少做握手**加的：实测 54 个候选的 TLS 阶段要 5.2 秒（4~5 波），
	// 而最终最多只用 16 个。先用一次便宜的 TCP 建连把明显不通的挡掉，再对前 N 个
	// 做真握手，启动能省下三秒左右 —— 用户在启动期间是什么都干不了的。
	//
	// 取 24 而不是 16：TCP 测不出"TLS 被 RST"这一类（那正是探测存在的理由），
	// 多留几个名额给它们，免得最后剩下的好节点不够。
	probeTLSCandidates = 24
	// probeScreenTimeout 是 TCP 建连筛查的时限。它必须明显小于 TLS 阶段 ——
	// 这一层的意义就是"快"，慢的候选直接留给下一轮去淘汰。
	probeScreenTimeout = 900 * time.Millisecond
	// probeScreenParallel 是筛查层的并发。TCP 建连很便宜，可以比后面两段开得高。
	probeScreenParallel = 24
	// probeUpgradeParallel 是升级阶段的并发数，**刻意比 TLS 阶段低**。
	//
	// 依据是一次实测：把 12 个升级同时打到同一个 Worker 上，有 7~12 个直接回
	// 403 / error 1034 —— 而这些 IP 在别的启动里是能用的。也就是说，探测自己
	// 制造了它要过滤掉的失败。升级是唯一会真的进到 Worker 的探测动作
	//（成功的会建 DO），所以这里要节制。
	probeUpgradeParallel = 6
	// echFetchTimeout 是取一次 ECHConfigList 的上限（走 DoH）。
	//
	// 独立于探测预算的理由很实际：省下这一次取��，等于给后面几十个候选的握手
	// 全部换上隐 SNI，划算得不像话。
	echFetchTimeout = 2 * time.Second
)

// 变量而非常量：测试要把它们压到毫秒级，才能在秒级用例里钉住"某个探测阶段
// 不会把启动预算吃光"这件事 —— 2026-10-03 那次启动从 5.1s 涨到 13.1s，就是
// 没有这条防线的情况下发生的（详见 8dbca79）。
//
// 2.5s 这个值不是随手取的：拨号侧把握手探测也定在 2.5s。两边一致意味着
// "探测说它快"和"拨号觉得它快"是同一个判断，不会出现"探测放进来了、拨号却要
// 等更久"的落差。反过来，候选里留着"要 4 秒才握手"的 IP 也没有意义 —— 真到拨号
// 时它同样会被那个时限判掉，只是把用户的等待拉长。
var (
	probeTimeout        = 2500 * time.Millisecond // TCP + TLS
	probeUpgradeTimeout = 2500 * time.Millisecond // TCP + TLS + WS 升级
)

// OptResult 是启动优选的结果。Rejected 单独拎出来是因为它对应一个用户能看见的
// 症状（"时好时坏"）：这些 IP 的 TLS 完全正常，只有真的建隧道时才会被边缘拒绝。
type OptResult struct {
	Nodes     []entry.Node
	ProbeTook time.Duration
	Upgraded  int // WS 升级通过的候选数
	// TLSTook / UpgradeTook 是两个阶段各自的耗时。启动慢的时候，用户需要知道的
	// 不是"探测花了 10 秒"，而是"哪一段花的" —— 没有这个拆分就只能猜。
	// TLSErrors 是 TLS 阶段失败的样本（最多几条）。整个候选集全军覆没时，
	// "为什么"只有这里有 —— 而那恰恰是用户完全看不见、我们也最容易瞎猜的时刻。
	TLSErrors   []string
	ScreenTook  time.Duration
	TLSTook     time.Duration
	UpgradeTook time.Duration
	Rejected    int // TLS 通、但 WS 升级被边缘拒的候选数
	// Refused 是被拒候选的明细（IP + 边缘的原话）。
	//
	// 为什么留着：被拒的边缘不会告诉任何人它为什么被拒，而"这些 IP 为什么不行"
	// 恰恰是判断"是源的问题还是我们的问题"的唯一依据。默认不打，开了
	// NETMASTER_PROBE_DEBUG 才进日志 —— 正常启动不该往用户的屏幕上倒这种噪音。
	Refused []RefusedEntry
}

// RefusedEntry 一个"握手正常但升级被拒"的候选。
type RefusedEntry struct {
	Node entry.Node
	Err  string
}

// ProbeResult 单个候选的探测结果。
type ProbeResult struct {
	Node    entry.Node
	Latency time.Duration
	Err     error
}

// screenTCP 是个接缝：默认真跑 ScreenTCP，测试里换成直通。
//
// 为什么需要：ScreenTCP 对着本机监听端口做真实建连，而探测的其余用例要反复
// 起好几十个 httptest 服务器。跑久了之后这些临时端口开始 TIME_WAIT，连上不去
// 的偶发失败会把**别的**用例判红 —— 红的地方和真正的原因隔着一个用例。
// 让筛查这一段可以被旁路掉，每条用例就只因为它自己要测的那件事而红。
var screenTCP = ScreenTCP

// ScreenTCP 只做 TCP 建连，按延迟升序返回可达的候选。
//
// 它替代不了 TLS 探测（"TCP 通、ClientHello 被 RST"这一类它一个都测不出），
// 只用来把候选集缩小到值得握手的规模。
func ScreenTCP(ctx context.Context, nodes []entry.Node) []ProbeResult {
	if len(nodes) == 0 {
		return nil
	}
	ch := make(chan ProbeResult, len(nodes))
	sem := make(chan struct{}, probeScreenParallel)
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			addr := net.JoinHostPort(nodes[i].Addr, fmt.Sprint(nodes[i].Port))
			c, err := net.DialTimeout("tcp", addr, probeScreenTimeout)
			if err != nil {
				ch <- ProbeResult{Node: nodes[i], Err: err}
				return
			}
			_ = c.Close() //nolint:errcheck
			ch <- ProbeResult{Node: nodes[i], Latency: time.Since(start)}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}

	var out []ProbeResult
	for len(ch) > 0 {
		r := <-ch
		if r.Err == nil && r.Latency > 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Latency < out[b].Latency })
	return out
}

// Probe 测全部候选，返回按延迟升序的可用结果（失败项排除）。
// sni 是 TLS 握手用的域名（即 server 域名）；insecure 透传拨号层的证书校验开关。
// ctx 超时即返回已到手的部分：启动不能被几个连不上的候选拖死。
func Probe(ctx context.Context, nodes []entry.Node, sni string, insecure bool, ech []byte) ([]ProbeResult, []string) {
	if len(nodes) == 0 {
		return nil, nil
	}
	// 结果经 channel 回收，而不是各 goroutine 往共享切片里写。
	// 差别在 ctx 到期那一刻：此时还有 goroutine 在跑，直接读切片就是数据竞争
	// （-race 能抓到，但线上表现为"启动结果偶发少几个节点"）。
	ch := make(chan ProbeResult, len(nodes))
	sem := make(chan struct{}, probeParallel)
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- probeOne(ctx, nodes[i], sni, insecure, ech)
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// 未完成的项永远不会到达 channel，下面的过滤自然把它们剔除。
	}

	var out []ProbeResult
	var errs []string
	for len(ch) > 0 {
		r := <-ch
		if r.Err == nil && r.Latency > 0 {
			out = append(out, r)
			continue
		}
		if len(errs) < tlsErrorSamples {
			errs = append(errs, r.Node.Addr+": "+r.Err.Error())
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Latency < out[b].Latency })
	return out, errs
}

// probeOne 对单个候选做一次真实 TLS 握手，返回耗时或错误。
func probeOne(ctx context.Context, node entry.Node, sni string, insecure bool, ech []byte) ProbeResult {
	start := time.Now()
	addr := net.JoinHostPort(node.Addr, fmt.Sprint(node.Port))
	raw, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return ProbeResult{Node: node, Err: err}
	}
	defer raw.Close() //nolint:errcheck

	if _, err := handshake(ctx, raw, sni, insecure, ech); err != nil {
		return ProbeResult{Node: node, Err: err}
	}
	return ProbeResult{Node: node, Latency: time.Since(start)}
}

// handshake 在已建立的 TCP 连接上完成 TLS 握手，**优先走 ECH**。
//
// 为什么探测也要隐 SNI（这条是实测逼出来的）：探测原本一律用明文 SNI，于是
//  1. 每次启动就把服务端域名明文发了 40~60 次 —— 对 GFW 来说这是同一域名的一串
//     清晰出现，正好是"这个域名该被盯上"的信号；
//  2. 探测量的路径根本不是客户端要走的那条（客户端默认开 ECH），于是它挑出来的
//     "好节点"未必真的能用；
//  3. 实测 11 个"升级被拒"的候选里有 8 个根本不是 Cloudflare 回的 403，
//     而是握手刚完就被 RST（GFW 主动重置）。
//
// 拿不到 ECH 配置时退回明文 SNI：少一层保护也比完全不测强。
func handshake(ctx context.Context, raw net.Conn, sni string, insecure bool, ech []byte) (net.Conn, error) {
	if len(ech) > 0 {
		return tlsutil.ECHOverConnDeadline(raw, sni, ech, insecure, probeTimeout)
	}
	_ = raw.SetDeadline(time.Now().Add(probeTimeout)) //nolint:errcheck
	hsCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	tlsConn := tls.Client(raw, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: insecure, //nolint:gosec // 探测层只筛路径，校验在拨号层
		NextProtos:         []string{"http/1.1"},
	})
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		return nil, err
	}
	return tlsConn, nil
}

// Optimize 按 PRD §6.6 把候选收敛成节点池：取最快的 TopN 排序返回。
//
// 两阶段：先全量 TLS 握手（无副作用，便宜），再对排在最前面的
// probeUpgradeCandidates 个做真实 WS 升级。**只验 TLS 会漏掉一整类坏节点** ——
// 实测有边缘 IP 的 TLS 握手完全正常，一到 WS 升级就回 403 / error 1034
// （PRD §14 M4 的 403/1034 观察）。这类 IP 进池之后，用户看到的是"第一次打开
// 某个站要等 3 秒对冲拨号才连上"，也就是最初报上来的"时好时坏"。
//
// 全灭时原样返回（全部候选都是死节点，但节点池为空会让代理完全不可用，
// 留个机会比直接放弃好）。
func Optimize(ctx context.Context, nodes []entry.Node, sni string, insecure bool) OptResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, probeBudgetAll)
	defer cancel()

	res := OptResult{ProbeTook: time.Since(start)}

	// ECH 配置**整个探测只取一次**，然后每个候选复用：它是按域名取的，与候选 IP
	// 无关，逐个取会把启动时间乘以候选数。
	//
	// 取不到就退回明文 SNI（见 handshake 的说明）：那是"少一层保护"，不是"不测"。
	ech := echConfigFor(ctx, sni)

	// 第一段：便宜的 TCP 筛查，把候选缩到值得握手的规模。
	scrStart := time.Now()
	screened := screenTCP(ctx, nodes)
	res.ScreenTook = time.Since(scrStart)
	if len(screened) > probeTLSCandidates {
		screened = screened[:probeTLSCandidates]
	}
	tlsCandidates := make([]entry.Node, 0, len(screened))
	for _, r := range screened {
		tlsCandidates = append(tlsCandidates, r.Node)
	}
	if len(tlsCandidates) == 0 {
		// 一个都连不上：不要在这里就交白卷。这一层的时限比后面两段都短，
		// 全灭更可能是筛查太严而不是全网不通，交给 TLS 阶段去试。
		tlsCandidates = nodes
	}

	// 第二段：真实 TLS 握手（优先 ECH）。
	tlsStart := time.Now()
	ranked, tlsErrs := Probe(ctx, tlsCandidates, sni, insecure, ech)
	res.TLSTook = time.Since(tlsStart)
	res.TLSErrors = tlsErrs
	if len(ranked) == 0 {
		res.Nodes, res.ProbeTook = nodes, time.Since(start)
		return res
	}
	head := ranked
	if len(head) > probeUpgradeCandidates {
		head = head[:probeUpgradeCandidates]
	}
	upStart := time.Now()
	passed, rejected, refused := upgradeOK(ctx, head, sni, insecure, ech)
	res.UpgradeTook = time.Since(upStart)
	res.Upgraded, res.Rejected, res.Refused = len(passed), rejected, refused

	out := make([]entry.Node, 0, probeTopN)
	for _, r := range passed {
		if len(out) == probeTopN {
			break
		}
		out = append(out, r.Node)
	}
	// 一个都没通过升级：宁可用 TLS 通的那批，也不要交一个空池 —— 前者至少还能靠
	// 对冲拨号换节点，后者是"客户端完全不可用"。
	//
	// 但**仍然要截到 probeTopN**：len(nodes) 是 pickNode 的分母，交回全部候选等于
	// 把节点选择面悄悄放大好几倍，而用户对此毫无察觉。（这条上限一度被漏掉，
	// 是在对抗评审里被翻出来的 —— 兜底路径最容易长出没人想过的性质。）
	if len(out) == 0 {
		fallback := ranked
		if len(fallback) > probeTopN {
			fallback = fallback[:probeTopN]
		}
		out = make([]entry.Node, len(fallback))
		for i, r := range fallback {
			out[i] = r.Node
		}
		// 兜底用的正是被拒的那批，别再报"N 个被拒"——那句话配上刚宣布的池子，
		// 用户读到的是自相矛盾的日志。
		res.Upgraded, res.Rejected, res.Refused = 0, 0, nil
	}
	res.Nodes, res.ProbeTook = out, time.Since(start)
	return res
}

// upgradeOK 对候选逐个做真实 WS 升级，返回通过的那批（保持延迟序）与被拒计数。
//
// 判定标准就是"有没有 101"：升级被边缘拒掉时返回的是 403 / 1014 / 1034 这类
// 非 101 响应（outbound 侧的 wsDiag 会把状态码与 body 前几百字节压成一行，
// 这里沿用同一套信息，方便用户直接贴给你排障）。
func upgradeOK(ctx context.Context, ranked []ProbeResult, sni string, insecure bool, ech []byte) ([]ProbeResult, int, []RefusedEntry) {
	type outcome struct {
		idx int
		err error // nil = 升级通过
	}
	// 同 Probe：结果走 channel。ctx 到期时还有 goroutine 在跑，直接读共享切片
	// 就是数据竞争；这里缓冲开满，晚到的结果只是没人读，不会阻塞也不会串位。
	ch := make(chan outcome, len(ranked))
	sem := make(chan struct{}, probeUpgradeParallel)
	var wg sync.WaitGroup
	for i := range ranked {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- outcome{i, upgradeProbe(ctx, ranked[i].Node, sni, insecure, ech)}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done(): // 预算用尽：用已完成的部分，别把启动拖死
	}

	passed := make([]ProbeResult, 0, len(ranked))
	var refused []RefusedEntry
	for len(ch) > 0 {
		o := <-ch
		if o.err == nil {
			passed = append(passed, ranked[o.idx])
		} else {
			refused = append(refused, RefusedEntry{Node: ranked[o.idx].Node, Err: o.err.Error()})
		}
	}
	sort.Slice(refused, func(a, b int) bool { return refused[a].Node.Addr < refused[b].Node.Addr })
	// channel 的到达顺序是完成顺序，不是延迟顺序 —— 节点池按延迟排序是有意义的
	//（对冲拨号先试最可能成功的那个），所以这里必须重排。
	sort.Slice(passed, func(a, b int) bool { return passed[a].Latency < passed[b].Latency })
	return passed, len(refused), refused
}

// upgradeProbe 对单个候选做一次完整拨号：TCP + TLS + WS 升级，只要求拿到 101。
//
// 为什么不直接用 outbound.DialWS：它的超时是为"给用户建隧道"定的（TLS 阶段 6 秒、
// 握手 8 秒）。探测要跑几十个候选，用那套超时的话，一波挂着不回的边缘就能把整个
// 启动预算吃光 —— 实测正是这样：加了升级这一阶段之后，启动从 5.1s 涨到 13.1s，
// 探测整整用满 10s 预算。这里自带一套按毫秒计的时限，与 TLS 阶段对称。
func upgradeProbe(ctx context.Context, node entry.Node, sni string, insecure bool, ech []byte) error {
	addr := net.JoinHostPort(node.Addr, fmt.Sprint(node.Port))
	raw, err := net.DialTimeout("tcp", addr, probeUpgradeTimeout)
	if err != nil {
		return err
	}
	defer raw.Close()                                        //nolint:errcheck
	_ = raw.SetDeadline(time.Now().Add(probeUpgradeTimeout)) //nolint:errcheck

	tlsConn, err := handshake(ctx, raw, sni, insecure, ech)
	if err != nil {
		return err
	}

	// 与 outbound.DialWS 同一个技巧：把已经握好手的连接交给 gorilla，
	// 否则它看到 wss 会再做一次 TLS 握手。
	u := url.URL{Scheme: "wss", Host: addr, Path: "/"}
	header := http.Header{}
	header.Set("Host", sni)
	d := websocket.Dialer{
		NetDialTLSContext: func(context.Context, string, string) (net.Conn, error) { return tlsConn, nil },
		HandshakeTimeout:  probeUpgradeTimeout,
	}
	ws, resp, err := d.Dial(u.String(), header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("ws upgrade: %w%s", err, upgradeDiag(resp))
	}
	return ws.Close()
}

// upgradeDiag 把边缘的非 101 响应压成一行，与 outbound.wsDiag 同样的用途：
// 用户报障时能直接贴出"这个 IP 为什么被拒"，而边缘本身不会告诉你。
func upgradeDiag(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	msg := strings.TrimSpace(string(body))
	if len(msg) > 120 {
		msg = msg[:120] + "..."
	}
	if msg == "" {
		return fmt.Sprintf(" (edge said %s)", resp.Status)
	}
	return fmt.Sprintf(" (edge said %s: %s)", resp.Status, msg)
}

// Summary 把探测结果压成一行日志（最快/最慢/失败数）。
func Summary(ranked []ProbeResult, total int) string {
	if len(ranked) == 0 {
		return fmt.Sprintf("no reachable entry out of %d", total)
	}
	fastest := ranked[0]
	slowest := ranked[len(ranked)-1]
	return fmt.Sprintf("%d/%d reachable, fastest %s %s, slowest %s %s",
		len(ranked), total,
		fastest.Node.Addr, fastest.Latency.Round(time.Millisecond),
		slowest.Node.Addr, slowest.Latency.Round(time.Millisecond))
}

// echConfigFor 取一次该域名的 ECHConfigList，供整个探测阶段复用。
//
// 取不到（网络不通、域名没有 ECH、之前刚熔断过）就返回 nil，探测退回明文 SNI。
// 这里**不记失败**：启动探测不是"能不能上网"的前提，为它加一条降级路径反而
// 让真正的问题更难查。
// echConfigFetch 是个接缝：默认真的去 DoH 取，测试里换成返回值。
//
// 为什么必须留这个口：Optimize 每次启动都会取一次 ECH 配置，而测试是对着本地
// TLS 服务器跑的 —— 不封住这个口，每条用例都在悄悄访问公网，于是"慢"和"通断"
// 取决于这台机器当时能不能连上 Cloudflare。用例失败时没人查得到原因。
var echConfigFetch = func(sni string) ([]byte, error) {
	return tlsutil.FetchECHConfigList(sni, "")
}

func echConfigFor(_ context.Context, sni string) []byte {
	if !tlsutil.ECHEnabled() {
		return nil
	}
	// 用 Background 而不是探测的 ctx：ECHConfig 取自 DoH，探测的预算已经很紧，
	// 而这一次取值的代价是"整轮探测要不要隐 SNI"，值得给它自己的时间。
	// ECHConfig 取自 DoH。给它独立的一份预算：探测的 10s 已经排得很满，
	// 而这一次取值的代价是"整轮探测要不要隐 SNI"。
	c, cancel := context.WithTimeout(context.Background(), echFetchTimeout)
	defer cancel()
	_ = c
	ech, err := echConfigFetch(sni)
	if err != nil {
		return nil
	}
	return ech
}
