package proxy

import (
	"bytes"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"netmaster/internal/rules"
)

// ---------------- 出口替身 ----------------

// stubPool 是一个可控的出口池。
//
// ⚠️ Len() 故意**不**等于 Alive()：Len()=49 / Alive()=0 复刻的是实测里那批
// "池子看着很满、其实全死"的节点。这两者混同正是"客户端打印 ready 而网页一张都
// 打不开"的成因，所以任何只问 Len() 的判据都会被这个替身抓住。
type stubPool struct {
	alive   int
	frag    map[string]bool
	proxied map[string]bool

	directDown map[string]bool
	dialFn     func(string) (net.Conn, error) // 非空时 Dial 走它（模拟"代理能用"）

	mu       sync.Mutex
	proxyTry int
}

func newStubPool(alive int) *stubPool {
	return &stubPool{alive: alive, frag: map[string]bool{}, proxied: map[string]bool{}, directDown: map[string]bool{}}
}

func (p *stubPool) Len() int   { return 49 }
func (p *stubPool) Alive() int { return p.alive }
func (p *stubPool) DialAuto(string) (net.Conn, bool, error) {
	return nil, false, errNoExit
}
func (p *stubPool) Dial(host string) (net.Conn, error) {
	if p.dialFn != nil {
		return p.dialFn(host)
	}
	return nil, errNoExit
}
func (p *stubPool) RetryProxy(host string) (net.Conn, error) {
	p.mu.Lock()
	p.proxyTry++
	p.mu.Unlock()
	c, err := net.Dial("tcp", "127.0.0.1:1")
	if err != nil {
		return nil, errNoExit
	}
	return c, nil
}
func (p *stubPool) NoteProxyFailure(string) {}

// NeedsFragDirect / NoteFragDirect / ForgetFragDirect 三个入口都必须归一化键，
// 与 selector.Pool 的做法一致。
//
// 这不是形式主义：proxy 侧拿到的是 CONNECT 目标 "example.com:443"，而记忆层读写
// 的键是不带端口的 "example.com"。两侧各自自洽（proxy 读写都带端口、selector 读写
// 都不带）恰恰是最容易漏的形式 —— 写进去的条目永远读不回来，记忆静默失效。
func (p *stubPool) key(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func (p *stubPool) NeedsFragDirect(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.frag[p.key(host)]
}
func (p *stubPool) NoteFragDirect(host string) {
	p.mu.Lock()
	p.frag[p.key(host)] = true
	p.mu.Unlock()
}
func (p *stubPool) ForgetFragDirect(host string) {
	p.mu.Lock()
	delete(p.frag, p.key(host))
	p.mu.Unlock()
}
func (p *stubPool) NoteProxyConfirmed(host string) {
	p.mu.Lock()
	p.proxied[p.key(host)] = true
	p.mu.Unlock()
}

// NoteDirectDown / DirectDownRecently 实现 proxy.DirectDownTracker（键归一化同上）。
func (p *stubPool) NoteDirectDown(host string) {
	p.mu.Lock()
	p.directDown[p.key(host)] = true
	p.mu.Unlock()
}
func (p *stubPool) DirectDownRecently(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.directDown[p.key(host)]
}

// ShouldTryFragDirect 实现 proxy.FragTryer：默认"值得先赌一次分片直连"，
// 已经在隧道上确认过的域名不再试（与 selector.Pool 的语义一致）。
func (p *stubPool) ShouldTryFragDirect(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.proxied[p.key(host)]
}
func (p *stubPool) proxyTries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.proxyTry
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

const errNoExit stubErr = "stub: no usable exit"

// testWriter 把服务端的路由日志并进测试输出：平时不吵，失败或 -v 时看得到。
// 这些用例的价值恰恰在于走了哪条分支，那条分支的日志就是最直接的证据。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// alwaysProxyRouter 让分流判给代理，于是 s.dial 才会走"没有出口 → 直连兜底"那条分支。
type alwaysProxyRouter struct{}

func (alwaysProxyRouter) Match(string, uint16) rules.Action { return rules.Proxy }

// alwaysDirectRouter 让分流判给直连，模拟 geoip CN / 用户 direct 规则命中的域名。
type alwaysDirectRouter struct{}

func (alwaysDirectRouter) Match(string, uint16) rules.Action { return rules.Direct }

// ---------------- 模拟 GFW 的服务端 ----------------

// dpiMode 是这个模拟阻断设备的三种立场。
//
// ⚠️ 这里曾经写成布尔量 allowFragmented，判据写成 `(reads > 3) == allowFragmented`。
// 那个组合在"明文到达 + allowFragmented=false"时算出**放行** —— 也就是 false 的
// 实际含义是"只放行明文"而不是"全封"。而"明文与分片都 RST"是测"分片失效应当
// 落回代理"所必需的形态，于是那条用例一直悄悄跑的是明文放行分支，测了个寂寞。
// 两态表达不了这个场景。
type dpiMode int

const (
	// dpiBlockAll 明文与分片都 RST。
	dpiBlockAll dpiMode = iota
	// dpiPlainOnly 只放行一次性写出的 ClientHello。
	dpiPlainOnly
	// dpiFragOnly 只放行被拆开写出的 ClientHello。
	dpiFragOnly
)

// dpiListener 复刻 GFW 的阻断行为：**按 ClientHello 到达的形态**决定放行还是 RST。
//
// 判据是"分几次读到的"，不是"内容是什么" —— 这正是分片技术利用的机制：TCP 有序，
// 阻断设备靠**重组**读出明文 SNI，而分片让它的重组窗口先到期。
type dpiListener struct {
	ln   net.Listener
	mode dpiMode
}

func newDPIListener(t *testing.T, mode dpiMode) *dpiListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &dpiListener{ln: ln, mode: mode}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *dpiListener) addr() string { return d.ln.Addr().String() }

func (d *dpiListener) serve(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 8192)
	reads, total := 0, 0
	deadline := time.Now().Add(5 * time.Second)

	for {
		if time.Now().After(deadline) {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, err := c.Read(buf)
		if n > 0 {
			reads++
			total += n
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// 分片的到达是断续的：安静一会儿就说明首段已经收齐。
				if total >= 32 {
					break
				}
				continue
			}
			return
		}
	}

	// 读次数 > 3 即"被拆开写过"。这是阻断设备能观察到的唯一信号。
	fragmented := reads > 3
	allowed := false
	switch d.mode {
	case dpiBlockAll:
		allowed = false
	case dpiPlainOnly:
		allowed = !fragmented
	case dpiFragOnly:
		allowed = fragmented
	}

	if allowed {
		if _, err := c.Write([]byte("SERVERHELLO")); err != nil {
			return
		}
		_, _ = io.Copy(c, c)
		return
	}
	// 阻断形态：一个字节都不回就断开。设 Linger(0) 让它是真的 RST 而不是 FIN ——
	// 形态更接近被墙，也顺带排除"客户端把 EOF 当成正常结束"这种侥幸。
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
}

// ---------------- 隧道驱动 ----------------

// clientHello 造一段形似 ClientHello 的字节：TLS 记录头 + 随机数 + 一段"扩展"。
func clientHello() []byte {
	b := make([]byte, 512)
	b[0] = 0x16 // handshake
	b[1] = 0x03
	b[2] = 0x01
	b[4] = 0x00
	copy(b[5:9], []byte{0x01, 0x00, 0x01, 0xfc})
	for i := 9; i < len(b); i++ {
		b[i] = byte(i % 251)
	}
	return b
}

const connEstablished = "HTTP/1.1 200 Connection Established"

// readHeader 读到 "\r\n\r\n" 为止。
//
// 刻意**不**按固定长度读：隧道失败时代理回的是 502，长度与 200 不同，
// 按固定长度读会一直等下去，把"断言失败"变成"测试挂死"。挂死的测试看不出
// 是什么坏了，而这条路径上正是要验证各种失败形态的。
func readHeader(c net.Conn) (string, error) {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for len(buf) < 256 {
		n, err := c.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
				return string(buf), nil
			}
		}
		if err != nil {
			return string(buf), err
		}
	}
	return string(buf), nil
}

// driveTunnel 把一个 CONNECT 隧道跑起来，模拟浏览器：发首段、收回应。
//
// 刻意走 s.tunnel 而不是直接调 relayWithReplay —— 后者会绕过 s.dial，
// 而"分片整条链路曾经一直惰性"这个缺陷正是那样藏住的：直接测内部函数时，
// 分流决策那一半根本没有被执行过。
//
// ⚠️ 返回前**必须**等隧道自己收尾。浏览器侧看到 EOF 只说明这一侧断了，不代表
// s.tunnel 已经跑完 —— 被墙那条路上它后面还有分片重试与代理改道，断言跑在前面
// 就会看到"还没发生的中间状态"。第一版就栽在这里：断言 proxy 没被调用，而
// 隧道当时还在分片重试的中途。
//
// settle 是"最多等到什么时候"。有回应就提前收工；一个慢目标本就不回数据，
// 那种用例要靠 settle 给它一个上界（要比 relayProbeWait 长，否则测的是探测窗口
// 而不是"慢 ≠ 被阻断"这个结论）。
func driveTunnel(t *testing.T, s *Server, host string, payload []byte, settle time.Duration) string {
	t.Helper()
	browser, proxySide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.tunnel(proxySide, host)
	}()

	hdrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		h, err := readHeader(browser)
		if err != nil {
			errCh <- err
			return
		}
		hdrCh <- h
	}()

	var hdr string
	select {
	case hdr = <-hdrCh:
	case err := <-errCh:
		browser.Close()
		t.Fatalf("reading the CONNECT response: %v", err)
	case <-time.After(10 * time.Second):
		browser.Close()
		t.Fatal("the tunnel never answered the CONNECT")
	}
	if !strings.HasPrefix(hdr, connEstablished) {
		browser.Close()
		<-done
		t.Fatalf("CONNECT answered %q, want %q", hdr, connEstablished)
	}

	go func() { _, _ = browser.Write(payload) }()

	conclusive := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc bytes.Buffer
		for {
			n, err := browser.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if bytes.Contains(acc.Bytes(), []byte("SERVERHELLO")) {
					break
				}
			}
			if err != nil {
				break // EOF：这一侧断了，等隧道自己收尾
			}
		}
		conclusive <- acc.Bytes()
	}()

	var got []byte
	select {
	case got = <-conclusive:
	case <-time.After(settle):
	}

	browser.Close()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Error("tunnel did not unwind after the browser closed")
	}
	return string(got)
}

func newServer(t *testing.T, pool *stubPool) *Server {
	t.Helper()
	return New(Config{
		Router:         alwaysProxyRouter{},
		Pool:           pool,
		DirectFallback: true,
		DialTimeout:    5 * time.Second,
		Logger:         log.New(testWriter{t}, "", 0),
		// 测试没法在 127.0.0.1 上占 443，所以这条链路（分片/重放/改道）的
		// HTTPS 判据在这里放宽成"任何端口"。
		IsHTTPS: func(string) bool { return true },
	})
}

// ---------------- P0-1：分流层真的把"尝试性直连"标出来了 ----------------

// liveAddr 返回一个保证拨得通的本机地址（只用来让 dial 成功，连接立刻被关）。
//
// 为什么不用假域名：dial 失败时 tentativeDirect 的取值无关紧要（调用方只看 err），
// 于是用 "blocked.example" 这类域名会把用例变成"断言 DNS 解析失败"——
// 它在有网和无网的机器上表现不同，而且一旦解析失败就悄悄 SKIP。
// P0-1 那条钉子绝不能是会被跳过的。
func liveAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// TestDialMarksFallbackAsTentative 是 P0-1 的钉子。
//
// 曾经 `s.dial` 的 tentativeDirect 是命名返回值而全函数从未被赋 true，
// 于是 relayWithReplay 的两个入口永假、约 250 行 TLS-RF 一次都不执行 ——
// 而当时 4 条测试全绿，因为它们都绕过了 s.dial。
func TestDialMarksFallbackAsTentative(t *testing.T) {
	s := newServer(t, newStubPool(0))
	c, mode, err := s.dial(liveAddr(t))
	if err != nil {
		t.Fatalf("dial to a live local address failed: %v", err)
	}
	defer c.Close()
	if !mode.direct() {
		t.Fatal("a direct dial taken because no exit is usable is by definition tentative; " +
			"returning exitProxy makes relayWithReplay unreachable")
	}
}

// TestDeadPoolStillTriggersDirectFallback 钉住"池里 49 个节点但全死"。
//
// 只问 Len() 的判据在这里返回 49，于是直连兜底永远不触发；实测表现为
// 客户端打印 ready、网页却一张都打不开，而失败原因不在任何用户能看到的地方。
func TestDeadPoolStillTriggersDirectFallback(t *testing.T) {
	pool := newStubPool(0)
	if pool.Len() == 0 {
		t.Fatal("the stub is supposed to look full (Len=49) while being entirely dead (Alive=0)")
	}
	s := newServer(t, pool)
	c, mode, err := s.dial(liveAddr(t))
	if err != nil {
		t.Fatalf("dial to a live local address failed: %v", err)
	}
	defer c.Close()
	if !mode.direct() {
		t.Fatalf("pool reports Len()=%d but Alive()=0; the direct fallback must still fire", pool.Len())
	}
}

// TestLivePoolStillTriesDirectFirst 钉住新契约（2026-10-04 改）：**池子健康时也要先赌
// 一次分片直连**。
//
// 依据：分流判 proxy 只说明这个站通常需要代理，不代表直连一定不通；而分片直连穿过去
// 就是 2 跳，绕隧道是 3 跳，差的是整个 Worker 出站那一段（实测 CF 托管页面 3–12s，
// 直连 1–2s）。用户装这个软件本身就说明普通直连不通，所以这里不问"明文行不行"，
// 直接带分片试。
func TestLivePoolStillTriesDirectFirst(t *testing.T) {
	pool := newStubPool(3) // 池子有可用出口
	s := newServer(t, pool)
	c, mode, err := s.dial(liveAddr(t))
	if err != nil {
		t.Fatalf("dial to a live local address failed: %v", err)
	}
	defer c.Close()
	if mode != directFrag {
		t.Fatal("有可用出口时也必须先赌一次分片直连 —— 否则分片那条路永远不会被尝试")
	}
}

// TestDirectAttemptFallsBackToPoolWhenTCPFails 是上一条的另一半：直连的 TCP 都建不起来
// （域名解析不了、黑洞、无路由）时，必须老老实实回池子，不能把错误抛给用户。
func TestDirectAttemptFallsBackToPoolWhenTCPFails(t *testing.T) {
	pool := newStubPool(3)
	s := newServer(t, pool)
	_, mode, err := s.dial("no-such-host.invalid:443")
	if err == nil {
		t.Fatal("the stub pool has no working exit; Dial should have failed")
	}
	if mode.direct() {
		t.Fatal("直连的 TCP 都建不起来时不能标记成直连 —— 那条路没什么可重放的")
	}
}

// ---------------- TLS-RF：分片直连真的穿得过去 ----------------

// TestFragmentedDirectRescuesABlockedHost 端到端：分片首段穿过去了。
//
// 这条用例是整套 TLS-RF 存在的理由。**新契约下它一次就成**：不需要任何先验记忆，
// 第一次尝试就是分片的（旧实现要先付一次明文被 RST 的探测窗口）。
func TestFragmentedDirectRescuesABlockedHost(t *testing.T) {
	dpi := newDPIListener(t, dpiFragOnly)
	pool := newStubPool(0)
	s := newServer(t, pool)
	host := hostOfTest(dpi.addr())
	if pool.NeedsFragDirect(host) {
		t.Fatal("precondition: no memory yet — the first attempt must stand on its own")
	}

	got := driveTunnel(t, s, dpi.addr(), clientHello(), 8*time.Second)

	if !strings.Contains(string(got), "SERVERHELLO") {
		t.Fatalf("got %q back through the tunnel; the fragmented direct never got through", got)
	}
	if pool.proxyTries() != 0 {
		t.Fatalf("fell back to the proxy %d time(s); fragmentation should have been tried first", pool.proxyTries())
	}
	if !pool.NeedsFragDirect(host) {
		t.Error("fragmentation worked but was not remembered; the next connection will pay the failed plain attempt again")
	}
}

// TestPlainOnlyDpiFallsBackToProxy 记下新契约的**代价**，免得将来有人以为它是漏的。
//
// dpiPlainOnly 模拟"只放行明文"的中间盒：分片被拒。旧实现先试明文、于是这种站点能直连；
// 新实现一律带分片起步，于是它落回代理。这是用户明确选的方向 —— "要是用户能普通直连
// 不会起这个软件"，代价（这类站点多绕一跳）比"每个新域名都先白等一个明文探测窗口"小。
//
// 真正兜住它的是记忆：落代理之后 directBlocked 记 30 分钟，那段时间不再付探测成本。
func TestPlainOnlyDpiFallsBackToProxy(t *testing.T) {
	dpi := newDPIListener(t, dpiPlainOnly)
	pool := newStubPool(0)
	s := newServer(t, pool)

	driveTunnel(t, s, dpi.addr(), clientHello(), 8*time.Second)

	if pool.proxyTries() == 0 {
		t.Fatal("分片被拒时必须落回代理（失败走隧道），而不是继续挂在直连上")
	}
	if pool.NeedsFragDirect(hostOfTest(dpi.addr())) {
		t.Error("分片失败了却记住了'分片可行'，后续每条连接都会白付分片延迟")
	}
}

// TestFragmentedFailureForgetsMemoryAndFallsBackToProxy 是上一条的失败侧。
//
// 明文被拦、分片也被拦时必须落回代理，并且**忘掉分片记忆** —— 那条记忆的优先级
// 高于"该走代理"，留着会让后续每条连接白付约 400ms 再落代理，持续 6 小时。
func TestFragmentedFailureForgetsMemoryAndFallsBackToProxy(t *testing.T) {
	dpi := newDPIListener(t, dpiBlockAll)
	pool := newStubPool(0)
	s := newServer(t, pool)
	hostPort := dpi.addr()
	host := hostOfTest(hostPort)

	// 先播下"这个域名要分片"的记忆，再让它失败。
	//
	// 顺序很关键：第一版让**第一次**连接去发现分片不可用，而那条路上记忆本来就是
	// 空的，于是"忘了没有"两种实现都观察不到 —— 变异验证直接报 MISSED。
	// 真正会漏掉的情形是：**已经记住了**，然后链路变了。
	pool.NoteFragDirect(host)

	driveTunnel(t, s, hostPort, clientHello(), 8*time.Second)

	if pool.proxyTries() == 0 {
		t.Fatal("both the plain and the fragmented direct were blocked; the proxy fallback must have been tried")
	}
	if pool.NeedsFragDirect(host) {
		t.Error("fragmentation failed but is still remembered; every later connection will pay for it")
	}
}

// TestBlockAllFallsBackToProxyAfterLearningFrag covers the other order:
// 分片是先学会的（这次也失败），所以代理兜底仍然必须发生。
func TestBlockAllFallsBackToProxyAfterLearningFrag(t *testing.T) {
	dpi := newDPIListener(t, dpiBlockAll)
	pool := newStubPool(0)
	s := newServer(t, pool)

	driveTunnel(t, s, dpi.addr(), clientHello(), 8*time.Second)

	if pool.proxyTries() == 0 {
		t.Fatal("plain and fragmented direct were both blocked; the proxy fallback must have been tried")
	}
	if pool.NeedsFragDirect(hostOfTest(dpi.addr())) {
		t.Error("fragmentation was never observed to work here, so it must not be remembered")
	}
}

// TestFragMemoryMakesTheNextConnectionSkipThePlainAttempt 钉住记忆的收益。
//
// 第一次探测到该域名要分片，第二次就该直接分片发出，不再浪费一次注定被 RST 的
// 明文尝试 —— 那次尝试的代价是真连接 + 一次 RST。
func TestFragMemoryMakesTheNextConnectionSkipThePlainAttempt(t *testing.T) {
	dpi := newDPIListener(t, dpiFragOnly)
	pool := newStubPool(0)
	s := newServer(t, pool)
	hostPort := dpi.addr()
	host := hostOfTest(hostPort)

	driveTunnel(t, s, hostPort, clientHello(), 8*time.Second)
	if !pool.NeedsFragDirect(host) {
		t.Fatal("first connection did not learn that this host needs fragmentation")
	}
	got := driveTunnel(t, s, hostPort, clientHello(), 8*time.Second)
	if !strings.Contains(string(got), "SERVERHELLO") {
		t.Fatalf("second connection got %q; a remembered host must go straight to fragmented", got)
	}
}

// ---------------- 不得误伤：目标只是慢 ----------------

// TestSlowTargetIsNotRerouted 钉住"不误伤"。
//
// ⚠️ 这是 2026-10-04 的一次**有意反转**，旧契约与此相反（那时断言 proxyTries()==0）。
//
// 旧契约：probeUp 超时只判"目标只是慢"，不据此改道 —— 理由是"超时不是被阻断的证据"。
// 新契约：超时按阻断处理，落隧道。
//
// 为什么反转：这条路径现在对**每个新域名**都要走一次（分片直连优先），而实测的阻断
// 形态里"TCP 连上、ClientHello 被静默丢弃"比 RST 更常见（BBC / Wikipedia 的明文直连
// 都是 20s 无响应，不是 RST）。把它当"慢"的后果是：请求被留在一条永远不会回应的直连
// 上，用户干等自己的超时 —— 实测 25s 以上，整页挂死。
//
// 代价如实记下：首字节真的超过 relayProbeWait 的目标会被推去代理，并记 30 分钟。
// 取舍依据：被墙网络里"静默丢弃"远多于"慢站点"，而多绕一跳远好过整页挂死。
//
// 顺带修掉旧用例的一个空断言：它手工构造 Config 而**没设 IsHTTPS**，于是
// 127.0.0.1:<随机端口> 根本不进重放路径（isHTTPSPort 只认 443），它断言的
// "没有改道"是必然成立的 —— 测了个寂寞。这里改用带 IsHTTPS 的 newServer。
func TestSilentDropFallsBackToProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { // 收下首段，然后一直不说话（静默丢弃的形态）
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				time.Sleep(6 * time.Second)
			}(c)
		}
	}()

	pool := newStubPool(0)
	s := newServer(t, pool)

	driveTunnel(t, s, ln.Addr().String(), clientHello(), 12*time.Second)

	if pool.proxyTries() == 0 {
		t.Error("对端一个字都不回时必须落隧道：留在直连上等于让用户干等自己的超时")
	}
}

func hostOfTest(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return h
}
