package tlsfrag

import (
	"bytes"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ---------------- 测试替身 ----------------

// recorder 记录每次 Write 收到的字节，用来断言"分片序列"而不只是最终内容。
//
// 为什么不能只断言最终字节：分片的全部意义在于**到达对端时的形态**。把两段内容
// 交错送达，对端拿到的是 A1 B1 A2 B2，拼不出任何一条合法记录 —— 而最终累加出来的
// 字节完全正确。只断言内容的话，这个 bug 是隐形的。
type recorder struct {
	mu   sync.Mutex
	recs [][]byte
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	r.recs = append(r.recs, cp)
	return len(p), nil
}

func (r *recorder) sizes() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.recs))
	for i, b := range r.recs {
		out[i] = len(b)
	}
	return out
}

// shortWriter 每次最多只接受 limit 字节，并且**不报错** ——
// io.Writer 契约明确允许 `n < len(p) && err == nil`（socket 发送缓冲满就是这种）。
type shortWriter struct {
	buf bytes.Buffer
	n   int // 本次调用最多接受的字节数
}

func (w *shortWriter) Write(p []byte) (int, error) {
	limit := w.n
	if limit > len(p) {
		limit = len(p)
	}
	return w.buf.Write(p[:limit])
}

// stalledWriter 报告 0 字节且不报错：契约上这是 writer 的 bug。
// 我们的写循环必须报错退出，而不是无限重试。
type stalledWriter struct {
	calls int
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, nil
}

// pipeConn 用 net.Pipe 造一对连接：写侧立刻能观察到字节与它们被切成了几段。
func pipeConn(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// ---------------- WriteWith ----------------

func TestWriteWithSplitsAndKeepsBytes(t *testing.T) {
	payload := bytes.Repeat([]byte("S"), 20)
	var rec recorder
	n, err := WriteWith(&rec, 4, 0, 12, payload)
	if err != nil {
		t.Fatalf("WriteWith: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("reported %d bytes written, want %d", n, len(payload))
	}
	var joined bytes.Buffer
	for _, b := range rec.recs {
		joined.Write(b)
	}
	if !bytes.Equal(joined.Bytes(), payload) {
		t.Fatalf("reassembled %q, want %q", joined.Bytes(), payload)
	}
	// 预算内 12 字节按 4 切片 = 3 片，加一次性尾巴 8 字节。
	if got, want := rec.sizes(), []int{4, 4, 4, 8}; len(got) != len(want) {
		t.Fatalf("fragment sizes %v, want %v", got, want)
	}
}

// TestWriteWithAdvancesByBytesActuallyWritten 钉住短写。
//
// 回退实现（游标按 chunk 而不是按实际写出的 n 推进）时，本用例看到的是：
// 20 字节只写出 13 字节，且内容是 SSSSSSSSSSS + 一段错位的尾巴。
func TestWriteWithAdvancesByBytesActuallyWritten(t *testing.T) {
	payload := bytes.Repeat([]byte("S"), 20)
	w := &shortWriter{n: 3}
	n, err := WriteWith(w, 4, 0, 12, payload)
	if err != nil {
		t.Fatalf("WriteWith: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("reported %d bytes written, want %d — short writes dropped bytes", n, len(payload))
	}
	if !bytes.Equal(w.buf.Bytes(), payload) {
		t.Fatalf("delivered %q, want %q", w.buf.Bytes(), payload)
	}
}

// TestWriteWithDoesNotSpinOnNoProgress 保证写循环不会挂死。
func TestWriteWithDoesNotSpinOnNoProgress(t *testing.T) {
	w := &stalledWriter{}
	done := make(chan error, 1)
	go func() {
		_, err := WriteWith(w, 4, 0, 12, bytes.Repeat([]byte("S"), 12))
		done <- err
	}()
	select {
	case err := <-done:
		if err != io.ErrShortWrite {
			t.Fatalf("got %v, want io.ErrShortWrite", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteWith spun on a writer that never accepts bytes")
	}
}

func TestWriteWithClampsSpan(t *testing.T) {
	payload := bytes.Repeat([]byte("S"), 8)
	for _, span := range []int{-1, -100} {
		var rec recorder
		n, err := WriteWith(&rec, 4, 0, span, payload)
		if err != nil {
			t.Fatalf("span=%d: %v", span, err)
		}
		if n != len(payload) {
			t.Fatalf("span=%d: reported %d, want %d", span, n, len(payload))
		}
		if got := len(rec.recs); got != 1 {
			t.Fatalf("span=%d: wrote %d pieces, want 1 (negative span means 'no fragmentation')", span, got)
		}
	}
}

// ---------------- Conn ----------------

func TestConnFragmentsUpToItsBudget(t *testing.T) {
	a, b := pipeConn(t)
	c := NewConnWith(a, 2, 0, 6)

	// net.Pipe 是同步且无缓冲的：每次 Read 只消费一次 Write。所以读侧必须循环读，
	// 否则写第二片时两边都在等 —— 测试替身自己的死锁，不是被测代码的问题。
	// 顺带把"到达了几次"记下来，那才是分片的证据。
	type res struct {
		data string
		read int
	}
	got := make(chan res, 1)
	go func() {
		var acc []byte
		reads := 0
		buf := make([]byte, 16)
		for len(acc) < 6 {
			n, err := b.Read(buf)
			if n > 0 {
				reads++
				acc = append(acc, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		got <- res{data: string(acc), read: reads}
	}()

	if _, err := c.Write([]byte("ABCDEF")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case r := <-got:
		if r.data != "ABCDEF" {
			t.Fatalf("read %q, want ABCDEF", r.data)
		}
		if r.read != 3 {
			t.Fatalf("delivered in %d reads, want 3 — the budget was not fragmented", r.read)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// TestConnDoesNotWriteAnEmptyTail 钉住"预算刚好覆盖整条数据"那一次多余的写。
//
// 真实 socket 上 Write(nil) 是一次白跑的系统调用；而分片的 ClientHello 有几十片，
// 也就是几十次白跑。回退实现时本用例报 "made 5 writes, want 4"。
func TestConnDoesNotWriteAnEmptyTail(t *testing.T) {
	a, b := pipeConn(t)
	// 抽干读侧，否则 net.Pipe 的写会阻塞。
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()

	counted := &countingConn{Conn: a}
	c := NewConnWith(counted, 1, 0, 4)

	if _, err := c.Write([]byte("ABCD")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := counted.writes(); got != 4 {
		t.Fatalf("made %d writes, want 4 — an empty trailing Write is still a syscall", got)
	}
}

// countingConn 数 Write 次数，用来抓"多写了一次"这类形状缺陷。
type countingConn struct {
	net.Conn
	mu sync.Mutex
	n  int
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *countingConn) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestConnSerializesConcurrentWrites 是这个包最重要的一条测试。
//
// 两条并发的 Write 各自拿到一段预算后，如果分片在 TCP 上交叉，对端看到的是
// A1 B1 A2 B2 而不是 A1 A2 B1 B2 —— 累加字节完全正确，但拼不出合法记录。
// crypto/tls 明确允许并发调用方法，所以这不是假想场景。
//
// ⚠️ 写这条测试时踩了两个坑，都记在这里因为它们都很容易再踩：
//
//  1. **不能用 net.Pipe。** 它同步且无缓冲，每写一片就要等一次读，于是 pipe 自己
//     把两条写串行化了 —— 回退掉写锁照样绿，替身当了锁。
//  2. **budget 必须大于单个 payload。** 否则先到的那条用光预算，后到的那条在
//     Write 入口就看到 budget<=0 而整个 payload 一次写出，根本不会分片。
//
// 即便如此，单轮的检出率仍不是 100%：实测回退写锁后 20 轮里 16 轮交错（10/16/5/4 runs），
// 4 轮恰好各跑各的。所以测试内部跑多轮 —— 漏检概率从 ~20% 降到可忽略。
// 这也是本仓库"新写的并发/时序测试要 -count>=6"的同一个道理，只是就地做在测试里。
func TestConnSerializesConcurrentWrites(t *testing.T) {
	const rounds = 8
	for r := 0; r < rounds; r++ {
		rec := &orderRecorder{}
		c := NewConnWith(&yieldingConn{rec: rec}, 1, 0, 16)

		done := make(chan struct{})
		var wg sync.WaitGroup
		for _, payload := range []string{"AAAAAAAA", "BBBBBBBB"} {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				_, _ = c.Write([]byte(p))
			}(payload)
		}
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: concurrent Writes did not finish", r)
		}

		if runs := rec.runs(); runs > 2 {
			t.Fatalf("round %d: fragmented into %d runs, want at most 2 — "+
				"concurrent Writes interleaved on the wire", r, runs)
		}
	}
}

// yieldingConn 有缓冲，且每次写完主动让出调度。
//
// 让出调度是关键：没有它，分片循环里的 Write 是一串无阻塞调用，两个 goroutine
// 可能各自跑完整个循环，交错就永远复现不了 —— 那这条测试就只是一次运气好的通过。
type yieldingConn struct {
	net.Conn // 只用到 Write，其余方法不会被调用
	rec      *orderRecorder
}

func (c *yieldingConn) Write(p []byte) (int, error) {
	c.rec.note(p)
	runtime.Gosched()
	return len(p), nil
}

// orderRecorder 统计"字节种类切换"的次数：A A A B B B 是 1 次，A B A B 是 3 次。
type orderRecorder struct {
	mu       sync.Mutex
	last     byte
	haveLast bool
	switches int
}

func (r *orderRecorder) note(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range p {
		if !r.haveLast {
			r.last, r.haveLast = ch, true
			continue
		}
		if ch != r.last {
			r.switches++
			r.last = ch
		}
	}
}

func (r *orderRecorder) runs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.haveLast {
		return 0
	}
	return r.switches + 1
}
