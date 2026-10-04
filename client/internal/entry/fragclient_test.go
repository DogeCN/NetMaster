package entry

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// clientHelloRecorder 用裸 TCP 监听器记录 ClientHello **在网线上是怎么到的**：
// 分成几次读、每次间隔多久。
//
// 为什么必须是裸 TCP：一旦在服务端做 TLS 握手，握手层就把分片重新组装了，
// 什么也观察不到 —— 而"重组前的形态"恰恰是分片技术的全部意义。
// 所以这里只收字节、不回应；客户端的握手会失败，但这条测试不关心结果，
// 它只关心 ClientHello 落到线上的形态。
type clientHelloRecorder struct {
	ln      net.Listener
	mu      sync.Mutex
	reads   int
	firstAt time.Time
	lastAt  time.Time
	// firstRead 是**第一次读到的字节数**。它比"总读次数"更难糊弄：分层错了
	// （先握手再打散）时第一次读就是整条 ClientHello；分片生效时第一次读只有一片。
	firstRead int
	done      chan struct{}
}

func newClientHelloRecorder(t *testing.T) *clientHelloRecorder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &clientHelloRecorder{ln: ln, done: make(chan struct{})}

	go func() {
		defer close(r.done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		for {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := c.Read(buf)
			r.mu.Lock()
			if n > 0 {
				now := time.Now()
				if r.reads == 0 {
					r.firstAt = now
					r.firstRead = n
				}
				r.lastAt = now
				r.reads++
			}
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return r
}

func (r *clientHelloRecorder) url() string {
	return fmt.Sprintf("https://%s/netmaster.txt", r.ln.Addr().String())
}

func (r *clientHelloRecorder) stats() (reads int, elapsed time.Duration, firstRead int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstAt.IsZero() {
		return r.reads, 0, 0
	}
	return r.reads, r.lastAt.Sub(r.firstAt), r.firstRead
}

func (r *clientHelloRecorder) close() {
	r.ln.Close()
	<-r.done
}

// TestFragClientFragmentsTheClientHelloOnTheWire 钉住分片订阅拉取的分层。
//
// 这条测试存在的原因是协作板 §9.2：那里记着"fragClient 的分层错了（已修）"，
// 而实测 HEAD 里从未修过 —— DialTLSContext 让 Transport 先完成整个 TLS 握手，
// ClientHello 早已一次性写出，随后包上的 tlsfrag.Conn 打散的是**握手之后**的
// HTTP 请求字节，对 SNI 阻断零作用。一条钉不住分层的测试，绿了也说明不了任何事。
//
// 回退到 DialTLSContext 时本用例报 "arrived in 1 read(s) over X" ——
// 字节一模一样，区别只在到达的形态，所以只能断言形态。
func TestFragClientFragmentsTheClientHelloOnTheWire(t *testing.T) {
	r := newClientHelloRecorder(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	// 握手必然失败（服务端不做 TLS），这里只关心它失败前把什么写上了线。
	_, _ = fetchSource(ctx, r.url())

	reads, _, firstRead := r.stats()
	r.close()

	if reads == 0 {
		t.Fatal("nothing reached the server; the test cannot tell a layering bug from a dial failure")
	}
	// 不分片时整个 ClientHello 是一次 Write 落地，服务端看到的是 1 次读。
	if reads < 3 {
		t.Fatalf("ClientHello arrived in %d read(s) — it was not fragmented "+
			"(a DialTLSContext here fragments only the bytes written after the handshake)", reads)
	}
	// 首片必须很小。
	//
	// 这里原先断言的是"各片之间有 ≥20ms 间隔"，理由是"必须拉开间隔才骗得过重组"。
	// 2026-10-04 的实测推翻了它：零延迟的 1B 分片（1B/0/400B）在同一网络、同一目标上
	// 3/3 通过、461ms，而带延迟的 1B/1ms/400B 是 1050ms —— 起作用的不是"间隔"，
	// 是"片足够小、前缀足够长"（见 tlsfrag.Chunk 的实测表）。于是默认 Delay 改为 0，
	// 这条断言也跟着换成"首片很小"，它同样是分层错误（先握手再打散）会立刻违反的性质。
	if firstRead >= 64 {
		t.Fatalf("the first read carried %d bytes — the ClientHello was not written piece by piece", firstRead)
	}
}

// TestFragClientDoesNotWriteTheFirstByteAsOnePiece 把"分片"与"只是多读了几次"分开：
// 首字节必须**独占**一次写，否则前 8 字节之后的一切仍然带着完整 SNI 落地。
func TestFragClientDoesNotWriteTheFirstByteAsOnePiece(t *testing.T) {
	r := newClientHelloRecorder(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, _ = fetchSource(ctx, r.url())
	reads, _, _ := r.stats()
	r.close()

	if reads == 0 {
		t.Fatal("nothing reached the server")
	}
	// 8 字节一片 => 一次 ClientHello 至少要 8 次读才能收齐 64 字节。
	if reads < 8 {
		t.Fatalf("only %d reads for the ClientHello; a single unsplit write would be 1", reads)
	}
}
