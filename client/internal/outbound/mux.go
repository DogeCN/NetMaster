package outbound

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// sleepMicro 短暂自旋等待（WS readLoop 在别处跑，这里只是等 buffer 填充）。
func sleepMicro(us int) { time.Sleep(time.Duration(us) * time.Microsecond) }

// sessionReadyTimeout 是等待服务端确认会话建立的上限。
// 必须大于服务端自身的出站连接超时（15s），否则会把"慢"误判成"没确认"。
const sessionReadyTimeout = 20 * time.Second

// parsePort 从 host:port 解析端口。
func parsePort(hostport string) (uint16, error) {
	_, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(n), nil
}

// MuxConn 是一条复用的 WS 连接，上面可以并发跑多个会话。
// 协议（与 server/src/protocol.js 对应）：
//   - 连接级：WS 建立后第一条消息是 16 字节 auth（DialWS/DialMuxPlain 已发送）
//   - 会话开帧：mux 帧，payload = [port][addrType][addr]
//   - 之后每帧：[1 字节 idLen][sessionId][payload]
type MuxConn struct {
	ws *websocket.Conn

	mu        sync.Mutex
	sessions  map[string]*MuxSession
	nextID    uint32
	closed    bool
	lastError string // 服务端最近一次回传的错误（控制帧）

	readOnce sync.Once
}

// MuxSession 是 MuxConn 上的一个逻辑会话，对上层表现为 net.Conn。
type MuxSession struct {
	mux       *MuxConn
	id        []byte
	key       string
	rbuf      []byte
	rmu       sync.Mutex
	wmu       sync.Mutex
	dead      bool
	err       string      // 服务端投递的失败原因（若有），优先于 EOF 暴露
	timer     *time.Timer // 会话级 deadline（不作用于共享 WS）
	cond      *sync.Cond  // 有新数据或会话结束时唤醒 Read
	ready     chan error  // 缓冲1：nil=服务端确认就绪，非nil=失败原因
	readyOnce sync.Once
}

// signalReady 通知 Open 该会话已在服务端建立。
func (s *MuxSession) signalReady() {
	s.readyOnce.Do(func() { s.ready <- nil })
}

// signalErr 通知 Open 该会话建立失败。
func (s *MuxSession) signalErr(msg string) {
	s.readyOnce.Do(func() { s.ready <- fmt.Errorf("mux session: %s", msg) })
}

func (s *MuxSession) setErr(msg string) {
	s.rmu.Lock()
	s.err = msg
	s.rmu.Unlock()
}

var muxConnCounter atomic.Uint32

// DialMux 建立一条复用的 WS 传输连接（DialWS 内已完成 TLS+WS+auth）。
func DialMux(c *Client) (*MuxConn, error) {
	wc, err := c.DialWS()
	if err != nil {
		return nil, err
	}
	return newMuxConn(wc.Raw()), nil
}

// DialMuxPlain 建立明文 WS 的 mux 连接并发送 auth（仅用于本地/测试对端，
// 不加密、不隐 SNI）。
func DialMuxPlain(wsURL string, auth []byte) (*MuxConn, error) {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, auth); err != nil {
		ws.Close()
		return nil, fmt.Errorf("send auth: %w", err)
	}
	return newMuxConn(ws), nil
}

func newMuxConn(ws *websocket.Conn) *MuxConn {
	m := &MuxConn{ws: ws, sessions: make(map[string]*MuxSession)}
	m.nextID = muxConnCounter.Add(1)
	go m.readLoop()
	return m
}

// readLoop 统一读取 WS，所有会话按 sessionId 分发。
func (m *MuxConn) readLoop() {
	for {
		_, data, err := m.ws.ReadMessage()
		if err != nil {
			m.logf("readLoop error: %v", err)
			m.closeAll(err)
			return
		}
		m.dispatch(data)
	}
}

// logf 调试日志（默认静默）。
var muxDebug bool

func (m *MuxConn) logf(format string, args ...interface{}) {
	if muxDebug {
		println(fmt.Sprintf("[mux] "+format, args...))
	}
}

func (s *MuxSession) logf(format string, args ...interface{}) {
	s.mux.logf(format, args...)
}

func (m *MuxConn) dispatch(data []byte) {
	if len(data) < 1 {
		return
	}
	idLen := int(data[0])
	// idLen == 0 是控制帧：[0x00][sidLen][sessionId][utf-8 错误文本]。
	// 会话帧的 id 恒为 4 字节且首字节非 0，故 0x00 可作标记。
	if idLen == 0 {
		m.handleControl(data)
		return
	}
	if len(data) < 1+idLen {
		return
	}
	key := string(data[1 : 1+idLen])
	payload := data[1+idLen:]

	m.mu.Lock()
	s := m.sessions[key]
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.deliver(payload)
}

func (m *MuxConn) closeAll(err error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	list := make([]*MuxSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.sessions = make(map[string]*MuxSession)
	m.mu.Unlock()
	for _, s := range list {
		s.markDead(err)
	}
}

// Open 新建一个到 target 的会话，复用这条 WS。
func (m *MuxConn) Open(target string) (net.Conn, error) {
	port, err := parsePort(target)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}

	// 每个会话一个唯一 id（worker 侧对重复 id 直接拒绝）。
	m.mu.Lock()
	m.nextID++
	next := m.nextID
	m.mu.Unlock()

	id := make([]byte, 4)
	binary.BigEndian.PutUint32(id, next)
	key := string(id)

	s := &MuxSession{mux: m, id: id, key: key, ready: make(chan error, 1)}
	s.cond = sync.NewCond(&s.rmu)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("mux: connection closed")
	}
	m.sessions[key] = s
	m.mu.Unlock()

	// 会话开帧：mux 帧，payload 是目标地址（sessionId 已在帧头，不再重复）。
	if err := m.writeFrame(id, sessionOpen(host, port)); err != nil {
		m.removeSession(key)
		return nil, err
	}

	// 等服务端确认连接真的建立了。
	// 这一步决定了上层能否如实回 "200 Connection Established"：若后端连不上，
	// 我们要能返回连接错误（浏览器显示"代理无法连接"），而不是让浏览器在
	// TLS 握手时收到空响应、误报"证书不可信"。
	// 超时视为失败：服务端的出站连接超时（15s）必然先到，20s 还没收到确认
	// 意味着这条传输已经出了别的问题，乐观返回只会把错误推迟成更难排查的
	// "连接成功但没有任何数据"。
	select {
	case err := <-s.ready:
		if err != nil {
			m.removeSession(key)
			return nil, err
		}
		return s, nil
	case <-time.After(sessionReadyTimeout):
		m.removeSession(key)
		return nil, fmt.Errorf("mux: no ready signal within %s", sessionReadyTimeout)
	}
}

func (m *MuxConn) removeSession(key string) {
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()
}

func (m *MuxConn) writeRaw(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("mux: connection closed")
	}
	return m.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (m *MuxConn) writeFrame(id []byte, payload []byte) error {
	buf := make([]byte, 0, 1+len(id)+len(payload))
	buf = append(buf, byte(len(id)))
	buf = append(buf, id...)
	buf = append(buf, payload...)
	return m.writeRaw(buf)
}

// Close 关闭整条 WS。
func (m *MuxConn) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	m.logf("Close() called — closing websocket")
	return m.ws.Close()
}

// LiveCount 返回当前活跃会话数（诊断用）。
func (m *MuxConn) LiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// ---- MuxSession 实现 net.Conn ----

func (s *MuxSession) deliver(payload []byte) {
	s.rmu.Lock()
	s.rbuf = append(s.rbuf, payload...)
	s.rmu.Unlock()
	s.cond.Broadcast()
}

func (s *MuxSession) markDead(err error) {
	s.rmu.Lock()
	if s.dead {
		s.rmu.Unlock()
		return
	}
	s.dead = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.rmu.Unlock()
	s.cond.Broadcast()
	s.mux.removeSession(s.key)
}

func (s *MuxSession) Read(b []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.rbuf) == 0 {
		if s.dead {
			// 服务端投递的失败原因优先于 EOF 暴露，便于定位。
			if s.err != "" {
				return 0, fmt.Errorf("mux session closed: %s", s.err)
			}
			if msg := s.mux.lastErrorText(); msg != "" {
				return 0, fmt.Errorf("mux session closed: %s", msg)
			}
			return 0, io.EOF
		}
		s.cond.Wait()
	}
	n := copy(b, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

func (m *MuxConn) lastErrorText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastError
}

// handleControl 处理服务端控制帧：[0x00][sidLen][sessionId][payload]。
// 空 payload = 会话已就绪；非空 = 失败原因。
// 就绪信号很重要：没有它，上层代理只能乐观地回 "200 Connection Established"，
// 之后后端连接失败就会在浏览器里表现为 TLS/证书错误，而不是连接错误。
func (m *MuxConn) handleControl(data []byte) {
	if len(data) < 2 {
		return
	}
	sidLen := int(data[1])
	if len(data) < 2+sidLen {
		return
	}
	sid := string(data[2 : 2+sidLen])
	msg := string(data[2+sidLen:])

	m.mu.Lock()
	s := m.sessions[sid]
	m.mu.Unlock()
	if s == nil {
		return
	}

	if msg == "" {
		m.logf("session %x ready", sid)
		s.signalReady()
		return
	}
	m.logf("server control sid=%x: %s", sid, msg)
	m.mu.Lock()
	m.lastError = msg
	m.mu.Unlock()
	s.setErr(msg)
	s.signalErr(msg)
	s.markDead(nil)
}

func (s *MuxSession) Write(b []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.rmu.Lock()
	dead := s.dead
	s.rmu.Unlock()
	if dead {
		return 0, fmt.Errorf("mux: session closed")
	}
	if err := s.mux.writeFrame(s.id, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (s *MuxSession) Close() error {
	s.rmu.Lock()
	if s.dead {
		s.rmu.Unlock()
		return nil
	}
	s.dead = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.rmu.Unlock()
	s.cond.Broadcast()
	s.mux.removeSession(s.key)
	return nil
}

func (s *MuxSession) LocalAddr() net.Addr  { return s.mux.ws.LocalAddr() }
func (s *MuxSession) RemoteAddr() net.Addr { return s.mux.ws.RemoteAddr() }

// SetDeadline / SetReadDeadline / SetWriteDeadline are session-scoped.
//
// They must NOT be forwarded to the shared WebSocket: a deadline on the socket
// applies to every session multiplexed over it, so one timing-out session would
// tear down unrelated connections (this actually happened — a 5s read deadline
// on one session killed the whole mux connection). Instead we arm a timer that
// closes only this session when it expires; Read then unblocks with EOF.
func (s *MuxSession) SetDeadline(t time.Time) error { return s.SetReadDeadline(t) }

func (s *MuxSession) SetReadDeadline(t time.Time) error {
	s.armTimer(t)
	return nil
}

func (s *MuxSession) SetWriteDeadline(t time.Time) error {
	return nil // writes go straight to the socket; no session-level gating
}

func (s *MuxSession) armTimer(t time.Time) {
	s.rmu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	deadline := t
	s.rmu.Unlock()

	if deadline.IsZero() {
		return
	}
	d := time.Until(deadline)
	if d <= 0 {
		s.Close()
		return
	}
	s.rmu.Lock()
	s.timer = time.AfterFunc(d, func() {
		s.logf("session %x deadline exceeded, closing session", s.key)
		s.Close()
	})
	s.rmu.Unlock()
}
