package selector

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"netmaster/internal/entry"
)

// IP 优选（PRD §6.6）：并行测候选入口的 TCP 建连延迟，取最快的 TopN 作为节点池
// 并按延迟排序；再随机选一个作为当前节点。
//
// 只测延迟不测吞吐：吞吐测试（多次小请求测带宽）在启动预算内做不完 16 个候选，
// 而延迟才是决定"握手卡不卡"的那一项。代价是延迟相近的节点之间无法区分真实
// 吞吐 —— 这在浏览器访问场景下影响有限。

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
// ctx 超时即返回已到手的部分：启动不能被几个连不上的候选拖死。
func Probe(ctx context.Context, nodes []entry.Node) []ProbeResult {
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
			c.Close()
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
func Optimize(ctx context.Context, nodes []entry.Node) ([]entry.Node, time.Duration) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, probeBudgetAll)
	defer cancel()

	ranked := Probe(ctx, nodes)
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
