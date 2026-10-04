package tlsfrag

import (
	"bytes"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// OOB 关闭（默认）时，即使写入方是支持 SyscallConn 的真连接，也走普通分片路径。
func TestOOBDisabledByDefault(t *testing.T) {
	old := OOB
	defer func() { OOB = old }()
	if OOB {
		t.Fatalf("OOB 默认应为 false（紧急字节是否入流取决于对端，不能默认开）")
	}
}

// OOB 开启但写入方不是 net.Conn（拿不到 RawConn）：必须静默退回普通写，
// 字节一个不少。这是"能力探测失败 = 普通分片"的契约。
func TestOOBFallbackOnPlainWriter(t *testing.T) {
	old := OOB
	OOB = true
	defer func() { OOB = old }()

	var buf bytes.Buffer
	payload := []byte(strings.Repeat("x", 40))
	n, err := WriteWith(&buf, 8, time.Millisecond, len(payload), payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) || !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("fallback 写入不完整: n=%d len=%d", n, buf.Len())
	}
}

// OOB 开启且是真 TCP 连接：对端（SO_OOBINLINE 关闭，各平台默认）应按普通流读到
// 完整的 payload —— 紧急字节走带外，不占流内位置。
func TestOOBSendOnRealConn(t *testing.T) {
	old := OOB
	OOB = true
	defer func() { OOB = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen: %v", err)
	}
	defer ln.Close()

	payload := []byte(strings.Repeat("A", 40))
	type result struct {
		got []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer c.Close()
		got := make([]byte, len(payload))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{got: got}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, ok := c.(interface {
		SyscallConn() (syscall.RawConn, error)
	}); !ok {
		t.Skip("net.Conn 不支持 SyscallConn")
	}
	if _, err := WriteWith(c, 8, time.Millisecond, len(payload), payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("server side: %v", r.err)
		}
		if !bytes.Equal(r.got, payload) {
			t.Fatalf("流内字节与 payload 不一致（紧急字节泄漏进了流？）")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timeout waiting for server")
	}
}
