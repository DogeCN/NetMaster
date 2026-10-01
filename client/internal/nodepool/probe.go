package nodepool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"netmaster/internal/cache"
	"netmaster/internal/entry"
	"netmaster/internal/outbound"
)

// ProbeCacheTTL 是探测结果的可用期：超过这个年龄就不再预置，宁可先空跑再探测。
const ProbeCacheTTL = 10 * time.Minute

// probeFailedLatency 给探测失败的节点记的延迟。足够大以在评分里靠后，
// 但不至于像熔断那样把它彻底排除（探测抖动不代表节点真的坏了）。
const probeFailedLatency = 3 * time.Second

// probeConcurrency 是并发探测数。太高会互相抢带宽、把 RTT 测得偏高。
const probeConcurrency = 12

// ProbeResult 是单节点的探测结果，可持久化供下次启动预置。
type ProbeResult struct {
	Addr string        `json:"addr"`
	Port uint16        `json:"port"`
	RTT  time.Duration `json:"rtt"`
	OK   bool          `json:"ok"`
}

// recordProbe 记录一次探测结果。
//
// 与 recordSuccess/recordFailure 的关键区别：**探测失败不计入 fail/consecutive**，
// 只反映为高延迟。探测是十几个并发一起跑的，抖动很常见；若按失败计，一次网络
// 抖动就能把若干本来健康的节点推到"连续失败 3 次"的熔断线上。
func (s *nodeState) recordProbe(d time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		if s.latency == 0 {
			s.latency = d
		} else {
			s.latency = time.Duration(float64(s.latency)*0.7 + float64(d)*0.3)
		}
		return
	}
	if s.latency < probeFailedLatency {
		s.latency = probeFailedLatency
	}
}

// ProbeAsync 在后台探测所有节点的握手延迟。
//
// 只更新各节点的延迟估计（nodeState 自带锁）与原子主节点索引，**不重排
// nodes/states 切片** —— 切片被 Dial/tryDial 并发读取，重排需要引入快照机制；
// 而排序的全部意义只是"让快节点优先"，评分函数本就基于延迟，所以更新延迟
// 即可达成同样效果，且没有数据竞争。
//
// 返回的通道在探测结束时收到结果，供调用方持久化（下次启动预置）。
func (p *Pool) ProbeAsync() <-chan []ProbeResult {
	done := make(chan []ProbeResult, 1)
	go func() { done <- p.probeAll() }()
	return done
}

// ProbeBlocking 同步探测，供 --wait-probe 与排查使用。
func (p *Pool) ProbeBlocking() []ProbeResult { return p.probeAll() }

// probeDialBudget 是单节点探测的硬预算。
//
// 为什么必须有：网络不可用时，一次 TCP 连接要一直等到超时（底层是 10s）。
// 17 个节点按 12 并发跑成两波，探测总时长因此会从正常时的 ~3s 恶化到 ~30s
// （实测看到过 23s / 28s / 29s）。而探测只是给节点排序，单个节点超过 4s 本来
// 也没有使用价值，不值得为它拖住整轮。实测正常网络下各节点在 0.5~2s 之间，
// 4s 有足够余量。
const probeDialBudget = 4 * time.Second

func (p *Pool) probeAll() []ProbeResult {
	out := make([]ProbeResult, len(p.nodes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeConcurrency)
	for i := range p.nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p.probeOne(out, i)
		}(i)
	}
	wg.Wait()

	if best := bestProbe(out); best >= 0 {
		atomic.StoreUint32(&p.idx, uint32(best))
	}
	return out
}

// probeOne 探测单个节点并写回结果与延迟估计，耗时以 probeDialBudget 为上限。
func (p *Pool) probeOne(out []ProbeResult, i int) {
	node := p.nodes[i]
	c := &outbound.Client{Node: node, UseECH: p.useECH, Insecure: p.insecure}

	type attempt struct {
		// 存具体指针类型而非 net.Conn 接口：DialWS 失败时返回 nil 的 *WSConn，
		// 一旦装进接口字段，接口本身就不为 nil（typed-nil），`conn != nil` 判断
		// 会通过，随后调用 Close() 触发 nil 解引用 panic。用具体类型则不会有这个陷阱。
		conn *outbound.WSConn
		err  error
	}
	ch := make(chan attempt, 1)
	start := time.Now()
	go func() {
		conn, err := c.DialWS()
		ch <- attempt{conn, err}
	}()

	record := func(d time.Duration, ok bool) {
		p.states[i].recordProbe(d, ok)
		out[i] = ProbeResult{Addr: node.Addr, Port: node.Port, RTT: d, OK: ok}
	}

	select {
	case a := <-ch:
		d := time.Since(start)
		if a.err == nil && a.conn != nil {
			a.conn.Close()
		}
		record(d, a.err == nil)
	case <-time.After(probeDialBudget):
		record(probeDialBudget, false)
		// 后台那次拨号可能稍后成功返回一个连接：这里没人接手，替它关掉，
		// 否则会一直挂到进程结束。
		go func() {
			a := <-ch
			if a.err == nil && a.conn != nil {
				a.conn.Close()
			}
		}()
	}
}

// bestProbe 返回探测成功且最快的那一项下标，全失败返回 -1。
func bestProbe(results []ProbeResult) int {
	best, bestRTT := -1, time.Duration(0)
	for i, r := range results {
		if !r.OK {
			continue
		}
		if best < 0 || r.RTT < bestRTT {
			best, bestRTT = i, r.RTT
		}
	}
	return best
}

// ApplyProbeCache 用上次的探测结果预置各节点的延迟估计与主节点。
//
// 目的是让启动后第一个请求就走对节点，而不必等本轮探测跑完（原来探测是阻塞的，
// 17 个节点要 5 秒，这段时间代理还没起来）。
// 返回命中的节点数。
func (p *Pool) ApplyProbeCache(results []ProbeResult) int {
	if len(results) == 0 {
		return 0
	}
	byKey := make(map[string]ProbeResult, len(results))
	for _, r := range results {
		byKey[probeKey(r.Addr, r.Port)] = r
	}

	hits := 0
	best, bestRTT := -1, time.Duration(0)
	for i, n := range p.nodes {
		r, ok := byKey[probeKey(n.Addr, n.Port)]
		if !ok {
			continue
		}
		hits++
		p.states[i].recordProbe(r.RTT, r.OK)
		if r.OK && (best < 0 || r.RTT < bestRTT) {
			best, bestRTT = i, r.RTT
		}
	}
	if best >= 0 {
		atomic.StoreUint32(&p.idx, uint32(best))
	}
	return hits
}

// Summary 把探测结果整理成一行可读的摘要。
func Summary(results []ProbeResult) string {
	ok := 0
	var fastest time.Duration
	var fastestAddr string
	for _, r := range results {
		if !r.OK {
			continue
		}
		ok++
		if fastest == 0 || r.RTT < fastest {
			fastest, fastestAddr = r.RTT, net.JoinHostPort(r.Addr, strconv.Itoa(int(r.Port)))
		}
	}
	if ok == 0 {
		return fmt.Sprintf("0/%d reachable", len(results))
	}
	return fmt.Sprintf("%d/%d reachable, fastest %s (%dms)", ok, len(results), fastestAddr, fastest.Milliseconds())
}

func probeKey(addr string, port uint16) string {
	return net.JoinHostPort(addr, strconv.Itoa(int(port)))
}

// SaveProbeCache 持久化探测结果。
func SaveProbeCache(nodes []entry.Node, results []ProbeResult) error {
	return cache.Save("probe", nodeSetID(nodes), results)
}

// LoadProbeCache 读取与当前节点集合匹配、且未过期的探测结果。
//
// 按节点集合哈希做键：换了订阅（或优选 IP 变了）之后旧延迟就没有参考价值，
// 用哈希天然隔离，不必自己比对。
func LoadProbeCache(nodes []entry.Node, maxAge time.Duration) ([]ProbeResult, bool) {
	var results []ProbeResult
	age, ok := cache.Load("probe", nodeSetID(nodes), &results)
	if !ok || len(results) == 0 || age > maxAge {
		return nil, false
	}
	return results, true
}

func nodeSetID(nodes []entry.Node) string {
	var sb strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&sb, "%s:%d,", n.Addr, n.Port)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:8])
}
