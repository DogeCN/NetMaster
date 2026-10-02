package outbound

// 端到端验证协议 v2 客户端 ↔ devserver（Node 同协议对端，见 server/test/devserver.mjs）。
// 测试自行拉起 devserver（node 子进程），无需预先手动启动；机器上没有 node 时跳过。

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// devserverPath 定位仓库内的 server/test/devserver.mjs（与测试包目录解耦）。
func devserverPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(thisFile, "..", "..", "..", "..", "server", "test", "devserver.mjs")
}

const devPassword = "devserver-password"

type devServer struct {
	url  string
	addr string // 127.0.0.1:<port>，devserver 监听地址
	cmd  *exec.Cmd
	done chan struct{}
	t    *testing.T
}

func startDev(t *testing.T) *devServer {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping devserver E2E")
	}
	cmd := exec.Command(node, devserverPath(), "0", devPassword, "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start devserver: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})

	dev := &devServer{cmd: cmd, done: done, t: t}
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "PORT=") {
				lineCh <- line
				continue
			}
			t.Log("dev:", line)
		}
	}()
	select {
	case line := <-lineCh:
		port := strings.TrimPrefix(line, "PORT=")
		dev.url = "ws://127.0.0.1:" + port + "/"
		dev.addr = "127.0.0.1:" + port
		return dev
	case <-time.After(15 * time.Second):
		t.Fatal("devserver did not report PORT in time")
		return nil
	}
}

// echoServer 原样回显的本地 TCP 服务，模拟任意流式目标。
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(c, c) //nolint:errcheck
				c.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

func httpGet(t *testing.T, conn net.Conn, host string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+host+"/", nil)
	req.Write(conn) //nolint:errcheck
	data, _ := io.ReadAll(io.LimitReader(conn, 64<<10))
	return string(data)
}

func TestProtoE2EEchoRoundtrip(t *testing.T) {
	dev := startDev(t)
	echo := echoServer(t)

	m, err := DialMuxPlain(dev.url, devPassword)
	if err != nil {
		t.Fatalf("dial mux: %v", err)
	}
	defer m.Close()

	conn, err := m.Open(echo)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer conn.Close()

	payload := []byte("hello over protocol v2")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("echo mismatch: %q", buf)
	}
}

func TestProtoE2EStatusCodes(t *testing.T) {
	dev := startDev(t)

	t.Run("wrong password rejected 0x01", func(t *testing.T) {
		m, err := DialMuxPlain(dev.url, "wrong-password")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer m.Close()
		if _, err := m.Open("127.0.0.1:9"); err == nil {
			t.Fatal("expected auth rejection")
		} else if !strings.Contains(err.Error(), "0x01") {
			t.Fatalf("expected 0x01, got: %v", err)
		}
	})

	t.Run("forbidden target 0x02", func(t *testing.T) {
		m, err := DialMuxPlain(dev.url, devPassword)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer m.Close()
		if _, err := m.Open("10.0.0.1:80"); err == nil {
			t.Fatal("expected forbidden")
		} else if !strings.Contains(err.Error(), "0x02") {
			t.Fatalf("expected 0x02, got: %v", err)
		}
	})

	t.Run("refused target 0x03", func(t *testing.T) {
		m, err := DialMuxPlain(dev.url, devPassword)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer m.Close()
		if _, err := m.Open("127.0.0.1:1"); err == nil {
			t.Fatal("expected exit failure")
		} else if !strings.Contains(err.Error(), "0x03") {
			t.Fatalf("expected 0x03, got: %v", err)
		}
	})
}

func TestProtoE2EConcurrentStreams(t *testing.T) {
	dev := startDev(t)
	echo := echoServer(t)

	m, err := DialMuxPlain(dev.url, devPassword)
	if err != nil {
		t.Fatalf("dial mux: %v", err)
	}
	defer m.Close()

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := m.Open(echo)
			if err != nil {
				errs[i] = fmt.Errorf("open: %w", err)
				return
			}
			defer conn.Close()
			payload := []byte(fmt.Sprintf("stream-%d-payload", i))
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Write(payload); err != nil {
				errs[i] = fmt.Errorf("write: %w", err)
				return
			}
			buf := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs[i] = fmt.Errorf("read: %w", err)
				return
			}
			if string(buf) != string(payload) {
				errs[i] = fmt.Errorf("mismatch: %q", buf)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("stream %d: %v", i, err)
		}
	}
	if got := m.LiveCount(); got != 0 {
		t.Errorf("live streams after test = %d, want 0", got)
	}
}

func TestProtoE2ELargePayload(t *testing.T) {
	dev := startDev(t)
	echo := echoServer(t)

	m, err := DialMuxPlain(dev.url, devPassword)
	if err != nil {
		t.Fatalf("dial mux: %v", err)
	}
	defer m.Close()

	conn, err := m.Open(echo)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	// 300KB：超过单帧上限，验证写路径分帧 + 读路径重组
	payload := make([]byte, 300*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	go func() {
		for off := 0; off < len(payload); off += MaxWriteChunk {
			end := off + MaxWriteChunk
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := conn.Write(payload[off:end]); err != nil {
				return
			}
		}
	}()
	got, err := io.ReadAll(io.LimitReader(conn, int64(len(payload))))
	if err != nil && len(got) < len(payload) {
		t.Fatalf("read: %v (got %d bytes)", err, len(got))
	}
	if len(got) != len(payload) {
		t.Fatalf("got %d bytes, want %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte %d mismatch: %d != %d", i, got[i], payload[i])
		}
	}
}

// MaxWriteChunk 写路径每次投喂的最大量（客户端 Write 有 64KB 上限断言）。
const MaxWriteChunk = 32 * 1024
