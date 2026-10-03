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
)

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
	results := make([]ProbeResult, len(nodes))
	sem := make(chan struct{}, probeParallel)
	var wg sync.WaitGroup
	for i := range nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			addr := net.JoinHostPort(nodes[i].Addr, fmt.Sprint(nodes[i].Port))
			c, err := net.DialTimeout("tcp", addr, probeTimeout)
			if err != nil {
				results[i] = ProbeResult{Node: nodes[i], Err: err}
				return
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
				results[i] = ProbeResult{Node: nodes[i], Err: err}
				return
			}
			results[i] = ProbeResult{Node: nodes[i], Latency: time.Since(start)}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// 未完成的项保持零值 Latency + nil Err，下面的过滤会剔除它们
		// （Err 为 nil 但没被写过的项 Latency 为 0，用 seen 标记区分更稳）。
	}

	var out []ProbeResult
	for _, r := range results {
		if r.Err == nil && r.Latency > 0 {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Latency < out[b].Latency })
	return out
}

// Optimize 按 PRD §6.6 把候选收敛成节点池：取最快的 TopN 排序返回。
// 全灭时原样返回（全部候选都是死节点，但节点池为空会让代理完全不可用，
// 留个机会比直接放弃好）。
func Optimize(ctx context.Context, nodes []entry.Node, sni string, insecure bool) ([]entry.Node, time.Duration) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, probeBudgetAll)
	defer cancel()

	ranked := Probe(ctx, nodes, sni, insecure)
	if len(ranked) == 0 {
		return nodes, time.Since(start)
	}
	if len(ranked) > probeTopN {
		ranked = ranked[:probeTopN]
	}
	out := make([]entry.Node, len(ranked))
	for i, r := range ranked {
		out[i] = r.Node
	}
	return out, time.Since(start)
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
