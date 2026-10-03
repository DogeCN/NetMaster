package selector

import (
	"sync"
	"testing"

	"netmaster/internal/entry"
)

// TestAddNodesGrowsAndDedups 钉住"候选表可以在启动之后继续增长"，且按 addr:port 去重。
//
// 为什么需要它：启动路径现在只用服务端 DNS 的候选建池（毫秒级），社区优选列表一两秒后
// 到，靠 AddNodes 并进来。这条路径一旦退化成"启动那一刻定终身"，表现是"社区源明明活着、
// 池子里却永远只有域名那几个 IP"，而且不会有任何报错。
//
// 端口用 1（必然拒绝）而不是不可达网段：拨号立刻失败，测试不必等超时。
func TestAddNodesGrowsAndDedups(t *testing.T) {
	p := New(Config{
		Nodes:     []entry.Node{{Addr: "127.0.0.1", Port: 1}},
		SNI:       "example.com",
		Password:  "pw",
		MuxTarget: 1,
	})
	defer p.Close()
	if got := p.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}

	added := p.AddNodes([]entry.Node{
		{Addr: "127.0.0.1", Port: 1}, // 已在池里
		{Addr: "127.0.0.1", Port: 2}, // 新
		{Addr: "127.0.0.1", Port: 3}, // 新
	})
	if added != 2 {
		t.Fatalf("added = %d, want 2 (the duplicate must be dropped)", added)
	}
	if got := p.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if added := p.AddNodes([]entry.Node{{Addr: "127.0.0.1", Port: 2}}); added != 0 {
		t.Fatalf("re-adding an existing node reported %d new, want 0", added)
	}
	if got := len(p.Nodes()); got != 3 {
		t.Fatalf("Nodes() = %d, want 3", got)
	}
}

// TestAddNodesConcurrentWithReaders 是给 -race 的钉子：AddNodes 的 append 与请求路径上的
// 读（Len / Nodes / pickNode / nodeAt）是并发的，任何一处漏锁都会在这里现形。
//
// 这条用例的价值全在 -race 上 —— 没有它，测试跑起来是绿的。
func TestAddNodesConcurrentWithReaders(t *testing.T) {
	p := New(Config{
		Nodes:     []entry.Node{{Addr: "127.0.0.1", Port: 1}},
		SNI:       "example.com",
		Password:  "pw",
		MuxTarget: 1,
	})
	defer p.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = p.Len()
				_ = p.Nodes()
				_ = p.Alive()
				if idx := p.pickNode(); idx >= 0 {
					_ = p.nodeAt(idx)
				}
			}
		}()
	}
	for i := 0; i < 40; i++ {
		p.AddNodes([]entry.Node{{Addr: "127.0.0.1", Port: uint16(1000 + i)}})
	}
	close(stop)
	wg.Wait()
}
