package proxy

import (
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"netmaster/internal/rules"
)

// newDirectServer 构造一个"分流全部判直连"的服务，模拟 geoip CN / 用户 direct 规则。
func newDirectServer(t *testing.T, pool *stubPool) *Server {
	t.Helper()
	return New(Config{
		Router:         alwaysDirectRouter{},
		Pool:           pool,
		DirectFallback: true,
		DialTimeout:    3 * time.Second,
		Logger:         log.New(testWriter{t}, "", 0),
		IsHTTPS:        func(string) bool { return true },
	})
}

// TestRulesDirectPlainStartThenFragRetry 钉住新契约（2026-10-04）：规则直连的域名
// 也进重放路径，且**明文起步** —— 被拦后阶梯②用分片补一枪，成功就留在直连。
//
// 曾经规则直连是裸 relay：SNI 被拦时用户浏览器的 TLS 直接死掉，整条"探测→分片→
// 改道"的机制对这条路径完全不生效，而且不写任何记忆，之后每个请求原样再死一次。
// dpiFragOnly 恰好是"明文被拦、分片能过"的形态，一步同时验证起步形态与阶梯②。
func TestRulesDirectPlainStartThenFragRetry(t *testing.T) {
	dpi := newDPIListener(t, dpiFragOnly)
	pool := newStubPool(0)
	s := newDirectServer(t, pool)
	host := hostOfTest(dpi.addr())

	got := driveTunnel(t, s, dpi.addr(), clientHello(), 8*time.Second)

	if !strings.Contains(string(got), "SERVERHELLO") {
		t.Fatalf("got %q; the plain-start ladder never reached the fragmented retry", got)
	}
	if pool.proxyTries() != 0 {
		t.Fatalf("fell back to the proxy %d time(s); the fragmented retry should have rescued the direct path", pool.proxyTries())
	}
	if !pool.NeedsFragDirect(host) {
		t.Error("fragmented retry worked but was not remembered; the next connection will be blocked again before retrying")
	}
}

// TestRulesDirectDownMemorySkipsRepeatDial 钉住负记忆：规则直连在 TCP 层失败后，
// 5 分钟内的后续请求不再重复付完整拨号超时，直接落代理。
func TestRulesDirectDownMemorySkipsRepeatDial(t *testing.T) {
	pool := newStubPool(3)
	s := newDirectServer(t, pool)
	dead := "127.0.0.1:1" // 拨号立刻被拒，不依赖网络条件

	// 第一次：直连 TCP 失败 → 记忆 + 错误照常上抛（池子的 Dial 也是死的）。
	if _, mode, err := s.dial(dead); err == nil {
		t.Fatal("direct dial to a dead port must fail while the pool is dead")
	} else if mode.direct() {
		t.Fatal("failed dial must not report a direct mode")
	}
	host := hostOfTest(dead)
	if !pool.DirectDownRecently(host) {
		t.Fatal("a TCP-level direct failure must leave a directDown memory")
	}

	// 第二次：池子活了。命中记忆 → 不再碰那个死端口，直接落代理成功。
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
			c.Close()
		}
	}()
	pool.dialFn = func(string) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) }

	c, mode, err := s.dial(dead)
	if err != nil {
		t.Fatalf("dial after memory should take the proxy shortcut: %v", err)
	}
	c.Close()
	if mode != exitProxy {
		t.Fatalf("the second dial must go through the proxy (mode=%d), not re-pay the doomed direct dial", mode)
	}
}

// TestRulesDirectDialFailureStillErrors 钉住另一半：负记忆**不**改变失败的结局。
// 直连和代理都失败时，错误照样上抛 —— 记忆省的是重复成本，不是失败的遮羞布。
func TestRulesDirectDialFailureStillErrors(t *testing.T) {
	pool := newStubPool(3) // 池子活着但 Dial 全失败
	s := newDirectServer(t, pool)
	dead := "127.0.0.1:1"

	if _, _, err := s.dial(dead); err == nil {
		t.Fatal("direct TCP failure with a dead pool must surface as an error (502)")
	}
	if !pool.DirectDownRecently(hostOfTest(dead)) {
		t.Fatal("the failure must still be remembered even when no proxy fallback succeeds")
	}
}

// 编译期守卫：stubPool 必须继续实现完整的能力面（router 假件别删）。
var (
	_ Router            = alwaysDirectRouter{}
	_ DirectDownTracker = (*stubPool)(nil)
	_                   = rules.Direct
)
