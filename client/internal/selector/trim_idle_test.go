package selector

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"netmaster/internal/entry"
	"netmaster/internal/outbound"
)

// muxHandler 是最小协议对端的实现：回 STATUS 0x00 后保持连接。
// 够用来造出真实的 MuxConn（空闲回调只在真实连接上才会触发）。
//
// 帧布局的关键点：连接上的**第一条消息是首帧** AUTH(16)|TS(8)|STREAM_ID(4)|ATYP|ADDR|PORT，
// stream id 在**偏移 24**；之后才是开帧 STREAM_ID(4)|ATYP|ADDR|PORT，id 在偏移 0。
// （写错这个偏移会让对端永远等不到响应——这个测试最初就是这么失败的。）
func muxHandler() http.Handler {
	up := websocket.Upgrader{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		first := true
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			off := 0
			if first {
				first = false
				off = 24 // 首帧
				if len(data) < 28 {
					continue
				}
			} else if len(data) < 5 {
				continue // 控制帧
			}
			id := int32(data[off])<<24 | int32(data[off+1])<<16 | int32(data[off+2])<<8 | int32(data[off+3])
			if id == 0 {
				continue
			}
			_ = c.WriteMessage(websocket.BinaryMessage, []byte{
				byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id), 0x00,
			})
		}
	})
}

func miniWS(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(muxHandler())
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func newTestPool() *Pool {
	return New(Config{
		Nodes: []entry.Node{{Addr: "127.0.0.1", Port: 443}, {Addr: "127.0.0.2", Port: 443}},
		SNI:   "example.com",
	})
}

// TestTrimIdleDropsSurplusMux 钉死 F1 级别的回归：空闲回收**必须真的接线**。
//
// 背景（A8 审计）：`MuxConn.OnIdle` 这个 setter 早就存在，但 attach() 从没注册过它，
// 于是 trimIdle 永不触发——而 PRD A12 与 limitations.md 当时已经把"空闲回收多余隧道"
// 写成已实施。文档承诺了代码没做的事，这类错误要有测试拦。
func TestTrimIdleDropsSurplusMux(t *testing.T) {
	srv, wsURL := miniWS(t)
	defer srv.Close()

	old := idleTrimDelay
	idleTrimDelay = 20 * time.Millisecond
	defer func() { idleTrimDelay = old }()

	m0, err := outbound.DialMuxPlain(wsURL, "pw")
	if err != nil {
		t.Fatalf("dial mux 0: %v", err)
	}
	m1, err := outbound.DialMuxPlain(wsURL, "pw")
	if err != nil {
		t.Fatalf("dial mux 1: %v", err)
	}
	defer m0.Close()
	defer m1.Close()

	p := newTestPool()
	p.attach(0, m0)
	p.attach(1, m1)
	if p.liveCount() != 2 {
		t.Fatalf("live count = %d, want 2", p.liveCount())
	}

	// 第一条上留一条开着的流：它不能被回收，于是唯一可能被回收的是第二条。
	//
	// （两条都空着的话，谁先到计时器谁走 —— attach 现在会给没有流的隧道也排计时器，
	// 这正是"备用隧道也能收缩"的代价：这条用例必须把另一条按住。）
	hold, err := m0.Open("example.com:443")
	if err != nil {
		t.Fatalf("open stream on mux 0: %v", err)
	}
	defer hold.Close()

	// 在第二条隧道上开一条流再关掉：最后一条流结束 → 空闲 → 应被回收
	st, err := m1.Open("example.com:443")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = st.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, stillThere := p.muxes[1]
		p.mu.Unlock()
		if !stillThere {
			return // 回收成功
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("idle surplus mux was never dropped — attach() is not wiring OnIdle (F1 regression)")
}

// TestTrimIdleKeepsLastMux 反向断言：只剩一条时不能回收，否则客户端会陷入
// "必须重连才能上网"。
func TestTrimIdleKeepsLastMux(t *testing.T) {
	srv, wsURL := miniWS(t)
	defer srv.Close()

	old := idleTrimDelay
	idleTrimDelay = 20 * time.Millisecond
	defer func() { idleTrimDelay = old }()

	m, err := outbound.DialMuxPlain(wsURL, "pw")
	if err != nil {
		t.Fatalf("dial mux: %v", err)
	}
	defer m.Close()

	p := newTestPool()
	p.attach(0, m)

	st, err := m.Open("example.com:443")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = st.Close()
	time.Sleep(200 * time.Millisecond) // 远大于 idleTrimDelay

	p.mu.Lock()
	_, stillThere := p.muxes[0]
	p.mu.Unlock()
	if !stillThere {
		t.Fatal("the last mux was dropped — the client would have to reconnect to browse")
	}
}
