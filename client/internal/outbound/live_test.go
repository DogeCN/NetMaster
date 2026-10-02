package outbound

// 真实部署验收（配合 .github/workflows/acceptance.yml）。只在 CI 里跑：需要
// NETMASTER_E2E_WORKER（已部署的 worker 主机名）与 NETMASTER_E2E_PASSWORD。
// 本地跑会被 Skip —— 它要连真实边缘，不是单元测试。

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"netmaster/internal/entry"
)

// liveClient 拨一个真实节点（优先 ECH；不可用则自动退普通 TLS）并建 mux。
func liveClient(t *testing.T, worker string) *MuxConn {
	t.Helper()
	client := &Client{
		Node:     entry.Node{Addr: worker, Port: 443, Name: worker},
		SNI:      worker,
		Password: os.Getenv("NETMASTER_E2E_PASSWORD"),
		UseECH:   true,
		Insecure: false,
	}
	m, err := DialMux(client)
	if err != nil {
		t.Fatalf("live dial failed: %v", err)
	}
	return m
}

func requireLive(t *testing.T) (string, bool) {
	t.Helper()
	worker := strings.TrimSpace(os.Getenv("NETMASTER_E2E_WORKER"))
	if worker == "" {
		t.Skip("NETMASTER_E2E_WORKER not set (CI-only acceptance test)")
	}
	if strings.TrimSpace(os.Getenv("NETMASTER_E2E_PASSWORD")) == "" {
		t.Skip("NETMASTER_E2E_PASSWORD not set")
	}
	return worker, true
}

// httpOverStream 通过一条逻辑流发一次 HTTP GET 并返回状态码。
// 目标若是明文 HTTP，隧道里就是裸 HTTP；这是验证链路最直接的方式。
// tlsOverStream 在一条已建立的隧道流上做标准 TLS 握手（真实证书校验）。
// Workers 的 connect() 禁拨 80 端口（平台返回 "consider using fetch"，m0 探针
// 实测），所以所有验收目标一律走 443。
func tlsOverStream(conn net.Conn, serverName string) (net.Conn, error) {
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName})
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	_ = tlsConn.SetDeadline(time.Time{})
	return tlsConn, nil
}

func httpOverStream(conn net.Conn, host, path string) (int, string, error) {
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nUser-Agent: netmaster-acceptance\r\n\r\n", path, host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	data, err := io.ReadAll(io.LimitReader(conn, 64<<10))
	if err != nil && len(data) == 0 {
		return 0, "", err
	}
	text := string(data)
	idx := strings.Index(text, "\r\n")
	if idx < 0 {
		return 0, text, fmt.Errorf("no status line in response (%d bytes)", len(data))
	}
	var code int
	fmt.Sscanf(text[:idx], "HTTP/1.1 %d", &code)
	return code, text, nil
}

// TestLive 直连出口 + 多路复用：同一个非 CF 托管目标，先单流后并发 20 条。
func TestLiveDirectExitAndMux(t *testing.T) {
	worker, ok := requireLive(t)
	if !ok {
		return
	}
	m := liveClient(t, worker)
	defer m.Close()

	// 目标非 CF 托管：验证 connect() 直连路径（不消耗 ProxyIP 槽位）。
	// 注意 example.com 在 2026 年已解析到 CF 网段（直连必被平台拒），选 Google。
	const target = "www.google.com:443"

	t.Run("single stream", func(t *testing.T) {
		conn, err := m.Open(target)
		if err != nil {
			t.Fatalf("open %s: %v", target, err)
		}
		defer conn.Close()
		tlsConn, err := tlsOverStream(conn, "www.google.com")
		if err != nil {
			t.Fatalf("tls handshake: %v", err)
		}
		defer tlsConn.Close()
		code, body, err := httpOverStream(tlsConn, "www.google.com", "/")
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if code != 200 {
			t.Fatalf("status = %d, want 200 (first 200 bytes: %q)", code, body[:min(200, len(body))])
		}
		t.Logf("direct exit OK: HTTP %d, %d bytes", code, len(body))
	})

	// 并发语义：真实浏览器会同时开几十条流。平台对单 DO 激活的同时出站连接数
	// 有限制（实测 20 条"打开成功"但立即 EOF），所以分两条路径测：
	//   单 WS 挤满：验证平台上限之内的行为（允许部分失败，只统计成功的绝对数）；
	//   4 条 WS 分摊：真实客户端的形态（selector 持多条 mux），要求全过。

	t.Run("20 concurrent streams on one ws", func(t *testing.T) {
		const n = 20
		var okN int32
		errCh := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				conn, err := m.Open(target)
				if err != nil {
					errCh <- fmt.Errorf("stream %d open: %w", i, err)
					return
				}
				defer conn.Close()
				tlsConn, err := tlsOverStream(conn, "www.google.com")
				if err != nil {
					errCh <- fmt.Errorf("stream %d tls: %w", i, err)
					return
				}
				defer tlsConn.Close()
				code, _, err := httpOverStream(tlsConn, "www.google.com", "/")
				if err != nil {
					errCh <- fmt.Errorf("stream %d request: %w", i, err)
					return
				}
				// Google 按地区可能回 302 重定向：隧道健在的证据就是拿到了响应
				if code < 200 || code >= 400 {
					errCh <- fmt.Errorf("stream %d status %d", i, code)
					return
				}
				atomic.AddInt32(&okN, 1)
				errCh <- nil
			}(i)
		}
		kinds := map[string]int{}
		for i := 0; i < n; i++ {
			if err := <-errCh; err != nil {
				kinds[trimErr(err.Error())]++
			}
		}
		for k, c := range kinds {
			t.Logf("failure x%d: %s", c, k)
		}
		got := int(atomic.LoadInt32(&okN))
		t.Logf("single-ws concurrent: %d/%d streams returned 200", got, n)
		if got == 0 {
			t.Errorf("single-ws concurrency: 0/%d succeeded — even below the platform limit", n)
		}
	})

	t.Run("20 concurrent streams spread over 4 ws", func(t *testing.T) {
		// selector 的真实形态：多条 mux 各自一个 Session DO，流分摊。
		const n = 20
		const wsCount = 4
		muxes := make([]*MuxConn, wsCount)
		for i := range muxes {
			mm := liveClient(t, worker) // 真实 TLS+ECH 拨号，一条 mux 一个 Session DO
			defer mm.Close()
			muxes[i] = mm
		}
		errCh := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				conn, err := muxes[i%wsCount].Open(target)
				if err != nil {
					errCh <- fmt.Errorf("stream %d open: %w", i, err)
					return
				}
				defer conn.Close()
				tlsConn, err := tlsOverStream(conn, "www.google.com")
				if err != nil {
					errCh <- fmt.Errorf("stream %d tls: %w", i, err)
					return
				}
				defer tlsConn.Close()
				code, _, err := httpOverStream(tlsConn, "www.google.com", "/")
				if err != nil {
					errCh <- fmt.Errorf("stream %d request: %w", i, err)
					return
				}
				if code != 200 {
					errCh <- fmt.Errorf("stream %d status %d", i, code)
					return
				}
				errCh <- nil
			}(i)
		}
		kinds := map[string]int{}
		fail := 0
		for i := 0; i < n; i++ {
			if err := <-errCh; err != nil {
				kinds[trimErr(err.Error())]++
				fail++
			}
		}
		for k, c := range kinds {
			t.Logf("failure x%d: %s", c, k)
		}
		t.Logf("spread concurrent: %d/%d streams returned 200", n-fail, n)
		if fail > 0 {
			t.Errorf("spread concurrency: %d/%d failed — multi-connection spreading did not hold", fail, n)
		}
	})
}

// TestLiveCFHosted 对 CF 托管目标连续请求，统计成功率（M3 验收项 ≥99%）。
// 走的是 ProxyIP 竞速路径 —— 直连 CF 网段会被平台拒绝。
func TestLiveCFHostedSuccessRate(t *testing.T) {
	worker, ok := requireLive(t)
	if !ok {
		return
	}
	rounds := 20
	if v := os.Getenv("NETMASTER_E2E_ROUNDS"); v != "" {
		fmt.Sscanf(v, "%d", &rounds)
	}

	// CF 托管目标 + 443 + TLS：直连 CF 网段必被平台拒，这一项验证的就是
	// ProxyIP 竞速路径的端到端成功率。
	const target = "www.cloudflare.com:443"

	m := liveClient(t, worker)
	defer m.Close()

	okCount := 0
	reasons := map[string]int{}
	for i := 0; i < rounds; i++ {
		conn, err := m.Open(target)
		if err != nil {
			reasons[trimErr(err.Error())]++
			continue
		}
		tlsConn, err := tlsOverStream(conn, "www.cloudflare.com")
		if err != nil {
			reasons[trimErr("tls: "+err.Error())]++
			conn.Close()
			continue
		}
		code, body, err := httpOverStream(tlsConn, "www.cloudflare.com", "/")
		tlsConn.Close()
		switch {
		case err != nil:
			reasons[trimErr(err.Error())]++
		case code >= 200 && code < 400:
			okCount++
		default:
			reasons[fmt.Sprintf("http %d: %q", code, body[:min(120, len(body))])]++
		}
		time.Sleep(300 * time.Millisecond)
	}

	rate := float64(okCount) / float64(rounds) * 100
	t.Logf("CF-hosted target %s: %d/%d ok (%.1f%%)", target, okCount, rounds, rate)
	for r, n := range reasons {
		t.Logf("  failure x%d: %s", n, r)
	}
	if rate < 99.0 {
		t.Errorf("success rate %.1f%% < 99%% target", rate)
	}
}

// trimErr 把错误压成一行便于聚类。
func trimErr(s string) string {
	if i := strings.Index(s, "("); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
