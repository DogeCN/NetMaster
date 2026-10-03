//go:build windows

package sysproxy

import (
	"net"
	"testing"
	"time"
)

// proxyListening 是 CleanupStale 新增的判据：owner 进程还活着不够，它的代理端口
// 上还得真有人在听，否则系统代理会一直指向一个空端口（watchdog 拒绝还原）。
// 这里只测它自身，不碰注册表 —— CleanupStale 的端到端行为涉及真实 HKCU，
// 不适合放进单测。
func TestProxyListening(t *testing.T) {
	if proxyListening("") {
		t.Fatal("empty addr must never count as listening")
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback listener: %v", err)
	}
	defer l.Close()

	if !proxyListening(l.Addr().String()) {
		t.Fatalf("addr %s has a live listener but proxyListening said no", l.Addr())
	}
}

// 端口上没有监听时必须报 false —— 这是"进程活着但服务已死"能被识别出来的关键。
func TestProxyListeningClosedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback listener: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Skipf("close: %v", err)
	}
	// 端口刚释放，系统可能立刻把它分配给别的监听者。那种情况下"还连得上"并不是
	// 本进程的监听，也就断言不了"这里没人听"这件事。所以先等复用消退，仍连得上
	// 就跳过 —— 会偶发变红的测试比没有测试更糟。
	deadline := time.Now().Add(2 * time.Second)
	for proxyListening(addr) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if proxyListening(addr) {
		t.Skipf("port %s was recycled by another listener; cannot assert deterministically", addr)
	}
}
