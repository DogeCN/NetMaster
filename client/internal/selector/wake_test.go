package selector

import (
	"testing"
	"time"
)

// TestAttachWakesPendingQueue 钉住"传输一挂上就唤醒等待队列"。
//
// 为什么需要它：队列自己是轮询的（退避 1s→2s→4s…），而 wait 只等 10s。没有这个
// 唤醒，排队的请求要等下一轮轮询 —— 那段时间里隧道明明已经好了，用户看到的却是
// 首屏卡住。实测的坏窗口里，这正是"第一个请求 9~20 秒"的组成部分之一。
//
// 用例刻意**不调 kick**：只有 attach → wake 这条路能把它叫醒。若唤醒被删掉，
// 这条用例会一直等到超时（而不是被轮询偶然救活）。
func TestAttachWakesPendingQueue(t *testing.T) {
	srv := miniWSTLS(t)
	defer srv.Close()
	host, port := srvAddr(t, srv)

	p := New(Config{
		Nodes:     repeatNodes(host, port, 1),
		SNI:       "example.com",
		Password:  "pw",
		Insecure:  true,
		MuxTarget: 1,
	})
	defer p.Close()

	req := &pendingReq{target: "example.com:443", ready: make(chan error, 1)}
	p.pending.mu.Lock()
	p.pending.queue = append(p.pending.queue, req)
	p.pending.mu.Unlock()

	if !p.redial() {
		t.Fatal("redial failed against the test server")
	}

	select {
	case err := <-req.ready:
		if err != nil {
			t.Fatalf("queued request settled with %v, want nil (transport is up)", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request was not woken by attach — it is still waiting for the next poll")
	}
}
