package proxy

import (
	"io"
	"log"
	"net"
	"testing"
	"time"
)

// blockingListener 接受连接后立刻关闭，一个字节都不回 —— 模拟 GFW 看到
// TLS ClientHello 里的明文 SNI 之后发 RST 的行为。
func blockingListener(t *testing.T) net.Listener {
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
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// echoListener 回显收到的数据，代表"改走代理之后能正常工作"的那一侧。
func echoListener(t *testing.T) net.Listener {
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
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func dialTo(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// 直连被阻断（零字节即断开）时，应改走代理并重放已缓存的首段数据。
func TestRelayWithReplayFailsOver(t *testing.T) {
	blocked := blockingListener(t)
	echo := echoListener(t)

	up := dialTo(t, blocked) // 这次"直连"会立刻被断
	client, peer := net.Pipe()
	defer peer.Close()

	retried := make(chan string, 1)
	srv := &Server{cfg: Config{
		Logger: newTestLogger(t),
		RetryViaProxy: func(host string) (net.Conn, error) {
			retried <- host
			return dialTo(t, echo), nil
		},
	}}

	go srv.relayWithReplay(client, up, "blocked.example:443", true)

	// 模拟客户端发出首个飞行段。
	payload := []byte("PING-CLIENTHELLO")
	go func() { _, _ = peer.Write(payload) }()

	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("no replayed echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", got, payload)
	}

	select {
	case h := <-retried:
		if h != "blocked.example:443" {
			t.Fatalf("retry target = %s, want blocked.example:443", h)
		}
	default:
		t.Fatal("expected a proxy failover, got none")
	}
}

// 直连正常（目标回了数据）时，不应触发任何回退。
func TestRelayWithReplayStaysDirect(t *testing.T) {
	echo := echoListener(t)
	up := dialTo(t, echo)
	client, peer := net.Pipe()
	defer peer.Close()

	srv := &Server{cfg: Config{
		Logger: newTestLogger(t),
		RetryViaProxy: func(string) (net.Conn, error) {
			t.Error("direct path was fine, but a proxy failover fired anyway")
			return nil, io.EOF
		},
	}}

	go srv.relayWithReplay(client, up, "ok.example:443", true)

	payload := []byte("HELLO")
	go func() { _, _ = peer.Write(payload) }()

	_ = peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("no direct echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q", got)
	}
}

type testLogger struct{ t *testing.T }

func (l testLogger) Write(p []byte) (int, error) {
	l.t.Logf("%s", p)
	return len(p), nil
}

func newTestLogger(t *testing.T) *log.Logger { return log.New(testLogger{t}, "", 0) }
