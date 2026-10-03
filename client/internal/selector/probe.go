package selector

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"netmaster/internal/entry"
	"netmaster/internal/outbound"
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
	probeTimeout   = 4 * time.Second
	probeParallel  = 12
	probeBudgetAll = 10 * time.Second // PRD §14 M4：优选全流程 ≤ 10s

	// probeUpgradeCandidates 是"WS 升级"这一阶段要验的候选数（按 TLS 延迟取前 N）。
	//
	// 为什么只验前 N 个而不是全部：升级一旦成功，请求就会真的进到 Worker 并
	// `SESSION.get()` 建一个 DO（见 server/src/index.js）。全量验 = 每次启动凭空
	// 建几十个用不上的 DO。取一个略高于 probeTopN 的前缀，是因为这个阶段本来
	// 就是"择优"：排在第 20 名之后的候选，就算升级能过也轮不到它进池。
	probeUpgradeCandidates = 24
	probeUpgradeTimeout    = 4 * time.Second
)

// OptResult 是启动优选的结果。Rejected 单独拎出来是因为它对应一个用户能看见的
// 症状（"时好时坏"）：这些 IP 的 TLS 完全正常，只有真的建隧道时才会被边缘拒绝。
type OptResult struct {
	Nodes     []entry.Node
	ProbeTook time.Duration
	Upgraded  int // WS 升级通过的候选数
	Rejected  int // TLS 通、但 WS 升级被边缘拒的候选数
}

// ProbeResult 单个候选的探测结果。
type ProbeResult struct {
	Node    entry.Node
	Latency time.Duration
	Err     error
}

// Probe 测全部候选，返回按延迟升序的可用结果（失败项排除）。
// sni 是 TLS 握手用的域名（即 server 域名）；insecure 透传拨号层的证书校验开关。
// ctx 超时即返回已到手的部分：启动不能被几个连不上的候选拖死。
func Probe(ctx context.Context, nodes []entry.Node, sni string, insecure bool) []ProbeResult {
	if len(nodes) == 0 {
		return nil
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
			ch <- probeOne(ctx, nodes[i], sni, insecure)
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
	for len(ch) > 0 {
		r := <-ch
		if r.Err == nil && r.Latency > 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Latency < out[b].Latency })
	return out
}

// probeOne 对单个候选做一次真实 TLS 握手，返回耗时或错误。
func probeOne(ctx context.Context, node entry.Node, sni string, insecure bool) ProbeResult {
	start := time.Now()
	addr := net.JoinHostPort(node.Addr, fmt.Sprint(node.Port))
	c, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return ProbeResult{Node: node, Err: err}
	}
	defer c.Close()                                 //nolint:errcheck
	_ = c.SetDeadline(time.Now().Add(probeTimeout)) //nolint:errcheck
	tlsConn := tls.Client(c, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: insecure, //nolint:gosec // 探测层只筛路径，校验在拨号层
		NextProtos:         []string{"h2", "http/1.1"},
	})
	hsCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		return ProbeResult{Node: node, Err: err}
	}
	return ProbeResult{Node: node, Latency: time.Since(start)}
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

	ranked := Probe(ctx, nodes, sni, insecure)
	if len(ranked) == 0 {
		res.Nodes, res.ProbeTook = nodes, time.Since(start)
		return res
	}
	head := ranked
	if len(head) > probeUpgradeCandidates {
		head = head[:probeUpgradeCandidates]
	}
	passed, rejected := upgradeOK(ctx, head, sni, insecure)
	res.Upgraded, res.Rejected = len(passed), rejected

	out := make([]entry.Node, 0, probeTopN)
	for _, r := range passed {
		if len(out) == probeTopN {
			break
		}
		out = append(out, r.Node)
	}
	// 一个都没通过升级：宁可用 TLS 通的那批，也不要交一个空池 —— 前者至少还能靠
	// 对冲拨号换节点，后者是"客户端完全不可用"。
	if len(out) == 0 {
		out = make([]entry.Node, len(ranked))
		for i, r := range ranked {
			out[i] = r.Node
		}
	}
	res.Nodes, res.ProbeTook = out, time.Since(start)
	return res
}

// upgradeOK 对候选逐个做真实 WS 升级，返回通过的那批（保持延迟序）与被拒计数。
//
// 判定标准就是"有没有 101"：升级被边缘拒掉时返回的是 403 / 1014 / 1034 这类
// 非 101 响应（outbound 侧的 wsDiag 会把状态码与 body 前几百字节压成一行，
// 这里沿用同一套信息，方便用户直接贴给你排障）。
func upgradeOK(ctx context.Context, ranked []ProbeResult, sni string, insecure bool) ([]ProbeResult, int) {
	type outcome struct {
		idx int
		err error // nil = 升级通过
	}
	// 同 Probe：结果走 channel。ctx 到期时还有 goroutine 在跑，直接读共享切片
	// 就是数据竞争；这里缓冲开满，晚到的结果只是没人读，不会阻塞也不会串位。
	ch := make(chan outcome, len(ranked))
	sem := make(chan struct{}, probeParallel)
	var wg sync.WaitGroup
	for i := range ranked {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- outcome{i, upgradeProbe(ctx, ranked[i].Node, sni, insecure)}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done(): // 预算用尽：用已完成的部分，别把启动拖死
	}

	passed := make([]ProbeResult, 0, len(ranked))
	rejected := 0
	for len(ch) > 0 {
		o := <-ch
		if o.err == nil {
			passed = append(passed, ranked[o.idx])
		} else {
			rejected++
		}
	}
	// channel 的到达顺序是完成顺序，不是延迟顺序 —— 节点池按延迟排序是有意义的
	//（对冲拨号先试最可能成功的那个），所以这里必须重排。
	sort.Slice(passed, func(a, b int) bool { return passed[a].Latency < passed[b].Latency })
	return passed, rejected
}

// upgradeProbe 对单个候选做一次完整拨号：TLS 握手 + WS 升级，只要求拿到 101。
func upgradeProbe(ctx context.Context, node entry.Node, sni string, insecure bool) error {
	c := &outbound.Client{Node: node, SNI: sni, Insecure: insecure}
	wc, err := c.DialWS()
	if err != nil {
		return err
	}
	return wc.Close()
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
