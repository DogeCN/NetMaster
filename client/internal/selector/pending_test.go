package selector

import (
	"fmt"
	"testing"
	"time"

	"netmaster/internal/entry"
)

func TestBackoffSequence(t *testing.T) {
	// PRD §6.7: 1s → 2s → 4s → 8s → 16s → 32s，每次 ±20% 抖动。
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 32 * time.Second,
	}
	for i, base := range want {
		attempt := i + 1
		for r := 0; r < 50; r++ {
			got := backoffFor(attempt)
			lo := time.Duration(float64(base) * 0.8)
			hi := time.Duration(float64(base) * 1.2)
			if got < lo || got > hi {
				t.Fatalf("backoffFor(%d) = %v, want within [%v, %v]", attempt, got, lo, hi)
			}
		}
	}
	// 上限：再往后仍然是 32s 封顶
	if got := backoffFor(20); got > time.Duration(float64(32*time.Second)*1.2) {
		t.Fatalf("backoffFor(20) = %v, exceeds 32s cap", got)
	}
	if got := backoffFor(0); got <= 0 {
		t.Fatalf("backoffFor(0) = %v", got)
	}
}

func TestPickNodeSkipsDeadAndResurrects(t *testing.T) {
	p := New(Config{Nodes: fakeNodes(4)})
	// 三个节点判死，只剩一个可用
	for i := 0; i < 3; i++ {
		p.dead[i] = true
	}
	for r := 0; r < 50; r++ {
		if idx := p.pickNode(); idx != 3 {
			t.Fatalf("pickNode = %d, want 3 (only live node)", idx)
		}
	}
	// 全判死 → 全部复活
	for i := range p.nodes {
		p.dead[i] = true
	}
	seen := map[int]bool{}
	for r := 0; r < 200; r++ {
		seen[p.pickNode()] = true
	}
	if len(seen) != 4 {
		t.Fatalf("after full death pickNode reached %d distinct nodes, want 4", len(seen))
	}
}

func TestNodeFailLimitMarksDead(t *testing.T) {
	p := New(Config{Nodes: fakeNodes(2)})
	for i := 0; i < nodeFailLimit-1; i++ {
		p.noteFailure(0)
		if p.dead[0] {
			t.Fatalf("node 0 died after %d failures, want %d", i+1, nodeFailLimit)
		}
	}
	p.noteFailure(0)
	if !p.dead[0] {
		t.Fatalf("node 0 should be dead after %d failures", nodeFailLimit)
	}
	if p.dead[1] {
		t.Fatal("node 1 must not be affected")
	}
	// noteSuccess 清零计数
	p.dead[1] = false
	p.fails[1] = 3
	p.noteSuccess(1)
	if p.fails[1] != 0 {
		t.Fatalf("fails[1] = %d after noteSuccess, want 0", p.fails[1])
	}
}

func TestPendingQueueFullRejected(t *testing.T) {
	p := New(Config{Nodes: fakeNodes(1)})
	// 直接填满队列（不启动重连，避免真实拨号）
	p.pending.mu.Lock()
	for i := 0; i < pendingLimit; i++ {
		p.pending.queue = append(p.pending.queue, &pendingReq{
			target: "example.com:443",
			ready:  make(chan error, 1),
		})
	}
	p.pending.mu.Unlock()
	if _, err := p.pending.enqueue("example.com:443"); err == nil {
		t.Fatal("enqueue beyond pendingLimit should fail")
	}
	p.pending.stop()
}

func TestPendingStopSettlesWaiters(t *testing.T) {
	p := New(Config{Nodes: fakeNodes(1)})
	req, err := p.pending.enqueue("example.com:443")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	p.pending.stop()
	select {
	case err := <-req.ready:
		if err == nil {
			t.Fatal("expected error after stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not settle the waiter")
	}
	if _, err := p.pending.enqueue("x:1"); err == nil {
		t.Fatal("enqueue after stop should fail")
	}
}

func TestPoolEmptyDialFails(t *testing.T) {
	p := New(Config{})
	if _, err := p.Dial("example.com:443"); err == nil {
		t.Fatal("dial with empty pool should fail fast")
	}
}

func fakeNodes(n int) []entry.Node {
	out := make([]entry.Node, n)
	for i := range out {
		out[i] = entry.Node{Addr: fmt.Sprintf("10.0.0.%d", i+1), Port: 443}
	}
	return out
}
