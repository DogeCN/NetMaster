package outbound

// 端到端验证 mux 客户端 ↔ Node devserver（跑真实 forward.js）。
// 需先启动：node test/devserver.mjs 8799 [password]
// 运行：DEV_WS=1 go test ./internal/outbound -run TestMuxE2E -v

import (
	"crypto/md5"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const devPassword = "devserver-password"

func requireDev(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping devserver test in -short mode")
	}
}

// TestMuxE2ESingle 单会话端到端取数据。
func TestMuxE2ESingle(t *testing.T) {
	requireDev(t)
	m, err := DialMuxPlain("ws://127.0.0.1:8799", devAuth())
	if err != nil {
		t.Skipf("devserver not running (%v) — start: node test/devserver.mjs 8799 %s", err, devPassword)
	}
	defer m.Close()

	conn, err := m.Open("127.0.0.1:8800")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer conn.Close()

	body := httpGet(t, conn)
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unexpected body: %q", body)
	}
	t.Logf("single session body: %s", body)
}

// TestMuxE2EConcurrent 多个会话并发跑在同一条 WS 上。
func TestMuxE2EConcurrent(t *testing.T) {
	requireDev(t)
	m, err := DialMuxPlain("ws://127.0.0.1:8799", devAuth())
	if err != nil {
		t.Skipf("devserver not running (%v)", err)
	}
	defer m.Close()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	bodies := make([]string, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := m.Open("127.0.0.1:8800")
			if err != nil {
				errs[i] = err
				return
			}
			defer conn.Close()
			// 交错发送：先写一点，等一会再写，模拟真实数据流
			if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				errs[i] = err
				return
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4096)
			total := 0
			for total < 10 {
				c, err := conn.Read(buf[total:])
				total += c
				if err != nil {
					if err == io.EOF {
						break
					}
					errs[i] = err
					return
				}
			}
			bodies[i] = string(buf[:total])
		}(i)
		time.Sleep(15 * time.Millisecond) // 依次发起，确保并发落在同一条 WS
	}
	wg.Wait()

	failCount := 0
	for i, err := range errs {
		if err != nil {
			failCount++
			t.Errorf("session %d error: %v", i, err)
			continue
		}
		if !strings.Contains(bodies[i], `"ok":true`) {
			failCount++
			t.Errorf("session %d bad body: %q", i, truncate(bodies[i], 80))
		}
	}
	if failCount == 0 {
		t.Logf("all %d concurrent sessions succeeded over ONE websocket (live=%d)", n, m.LiveCount())
	}
}

// TestMuxE2ESequentialReuse 连续多次 Open，复用同一条 WS。
func TestMuxE2ESequentialReuse(t *testing.T) {
	requireDev(t)
	m, err := DialMuxPlain("ws://127.0.0.1:8799", devAuth())
	if err != nil {
		t.Skipf("devserver not running (%v)", err)
	}
	defer m.Close()

	for i := 0; i < 5; i++ {
		conn, err := m.Open("127.0.0.1:8800")
		if err != nil {
			t.Fatalf("round %d open: %v", i, err)
		}
		body := httpGet(t, conn)
		if !strings.Contains(body, `"ok":true`) {
			t.Fatalf("round %d bad body: %q", i, truncate(body, 80))
		}
		conn.Close()
	}
	t.Log("5 sequential sessions reused one websocket")
}

func httpGet(t *testing.T, conn interface{ Write([]byte) (int, error) }) string {
	t.Helper()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	rc, ok := conn.(interface {
		Read([]byte) (int, error)
		SetReadDeadline(time.Time) error
	})
	if !ok {
		t.Fatalf("conn does not support read")
	}
	_ = rc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	total := 0
	for {
		c, err := rc.Read(buf[total:])
		total += c
		if err != nil {
			break
		}
		if total >= len(buf) {
			break
		}
	}
	raw := string(buf[:total])
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		return raw[i+4:]
	}
	return raw
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// devAuth 返回 devserver 默认口令派生的鉴权字节。
func devAuth() []byte {
	auth := md5.Sum([]byte(devPassword))
	return auth[:]
}
