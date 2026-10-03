package outbound

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"netmaster/internal/proto"
)

// MuxConn 是一条协议 v2 的复用 WebSocket 连接（PRD §4）。
//
// 帧分发：客户端→服务端的方向上，"未知流上的帧 = 开帧"（客户端在收到该流的
// STATUS 0x00 之前不发数据，见 protocol.js 头注释）；服务端→客户端方向上，
// 等待响应的流上第一帧是响应帧（5 字节），已就绪的流上全是数据帧。
// 控制帧以 0x00000000 前缀独立成类。
type MuxConn struct {
	ws       *websocket.Conn
	password string

	mu      sync.Mutex
	streams map[uint32]*MuxStream
	nextID  uint32
	closed  bool

	// 首帧必须先于任何开帧抵达服务端：服务端把"未认证时到达的第一帧"当首帧解析，
	// 一条开帧抢在首帧前面会被判成 HMAC 错误并断开整条连接。写路径统一走
	// writeOrdered，由它保证首帧只发一次且最先发。
	writeMu   sync.Mutex
	firstSent bool

	alive atomic.Bool
	// recycled 标记"服务端主动回收"（close(1000,"budget")：子请求预算见底，
	// 每条连接一生约 30 次出站建连）。这不是故障——不计入节点失败，并值得
	// 立刻在后台把下一条连接备好，避免下一波请求（浏览器一次开几十条流）
	// 全挤在重连的关键路径上。
	recycled atomic.Bool
	// onDead 在传输终止时回调（planned=true 表示服务端主动回收）。
	onDead func(planned bool)
	// onIdle 在最后一条流结束时回调（空闲信号，用于回收多余隧道）。
	onIdle func()
	// ping/pong 判死（PRD §4.6）：30s 一发，连续 2 个周期没 Pong 判死。
	lastPong atomic.Int64
	pingOnce sync.Once
}

// streamReadyTimeout 是等待服务端响应帧的上限。
// 必须大于服务端自身的出站连接超时（15s），否则会把"慢"误判成"没确认"。
const streamReadyTimeout = 20 * time.Second

var (
	errMuxClosed = errors.New("mux: connection closed")
	muxConnSeq   atomic.Uint32
	muxDebug     bool
)

// logf 调试日志（默认静默；muxDebug 由测试或 -tags 打开）。
func (m *MuxConn) logf(format string, args ...interface{}) {
	if muxDebug {
		println(fmt.Sprintf("[mux] "+format, args...))
	}
}

// DialMux 建立 TLS+WS 传输并返回 mux 连接。首帧（含认证）由第一次 Open 发出。
func DialMux(c *Client) (*MuxConn, error) {
	wc, err := c.DialWS()
	if err != nil {
		return nil, err
	}
	return NewMuxConn(wc.Raw(), c.Password), nil
}

// DialMuxPlain 建立明文 WS 的 mux 连接（本地/测试对端，不加密不隐 SNI）。
func DialMuxPlain(wsURL, password string) (*MuxConn, error) {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := d.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	return NewMuxConn(ws, password), nil
}

// NewMuxConn 包装一条已升级的 WS 连接。
func NewMuxConn(ws *websocket.Conn, password string) *MuxConn {
	m := &MuxConn{
		ws:       ws,
		password: password,
		streams:  make(map[uint32]*MuxStream),
		// 各连接错开起始 ID，降低多连接场景下 ID 撞车的心智负担（0 与
		// 0xFFFFFFFF 由 allocID 跳过）。
		nextID: muxConnSeq.Add(1),
	}
	m.alive.Store(true)
	m.lastPong.Store(time.Now().UnixNano())
	ws.SetPongHandler(func(string) error {
		m.lastPong.Store(time.Now().UnixNano())
		return nil
	})
	go m.readLoop()
	m.pingOnce.Do(func() { go m.pingLoop() })
	return m
}

func (m *MuxConn) Alive() bool { return m.alive.Load() }

// OnDead 注册传输终止回调（planned=true = 服务端主动预算回收，非故障）。
// 回调在独立 goroutine 里触发，不要在里面做阻塞操作。
func (m *MuxConn) OnDead(fn func(planned bool)) { m.onDead = fn }

// LiveCount 返回当前活跃流数（诊断用）。
func (m *MuxConn) LiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// pingLoop 每 30s 发一个 WS 协议层 Ping；边缘自动 Pong 且不唤醒 DO（M0 结论：
// 协议层 Ping 不产生 DO 消息，免费）。连续 2 个周期未收到 Pong 判死。
func (m *MuxConn) pingLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		if !m.alive.Load() {
			return
		}
		if time.Since(time.Unix(0, m.lastPong.Load())) > 65*time.Second {
			m.logf("pong missed for 2 periods, marking dead")
			m.kill(fmt.Errorf("mux: pong timeout"))
			return
		}
		m.mu.Lock()
		ws := m.ws
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return
		}
		_ = ws.WriteControl(websocket.PingMessage, []byte("nm"), time.Now().Add(5*time.Second))
	}
}

// allocID 严格递增，跳过 0 与 0xFFFFFFFF，到 0xFFFFFFFE 回绕到 1。
func (m *MuxConn) allocID() uint32 {
	for {
		id := m.nextID
		m.nextID++
		if m.nextID > proto.StreamIDMax {
			m.nextID = 1
		}
		switch id {
		case 0, 0xFFFFFFFF:
			continue
		}
		return id
	}
}

// Open 打开一条到 target 的逻辑流，等到服务端响应帧才算建立成功。
func (m *MuxConn) Open(target string) (net.Conn, error) {
	host, port, err := proto.SplitTarget(target)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errMuxClosed
	}
	id := m.allocID()
	s := newMuxStream(m, id)
	m.streams[id] = s
	m.mu.Unlock()

	if err := m.writeOrdered(id, host, port); err != nil {
		m.remove(id)
		return nil, err
	}

	// 等响应帧：这一步决定上层能否如实回 "200 Connection Established"。
	select {
	case err := <-s.ready:
		if err != nil {
			m.remove(id)
			return nil, err
		}
		return s, nil
	case <-time.After(streamReadyTimeout):
		m.remove(id)
		return nil, fmt.Errorf("mux: no stream response within %s", streamReadyTimeout)
	}
}

func (m *MuxConn) remove(id uint32) {
	m.mu.Lock()
	delete(m.streams, id)
	empty := len(m.streams) == 0
	m.mu.Unlock()
	// 最后一条流结束：通知上层"我闲下来了"。空闲是多条隧道里最该被回收的那条
	// ——服务端那侧的判死定时器会一直挂着阻止 DO 休眠（m0 E4/E9），空闲期全程
	// 按 DO 时长计费，而突发流量期的并发余量只在有流量时才有意义。
	if empty && m.onIdle != nil {
		go m.onIdle()
	}
}

// OnIdle 注册"最后一条流结束"的回调（用于回收多余的空闲隧道）。
func (m *MuxConn) OnIdle(fn func()) { m.onIdle = fn }

// writeOrdered 发开帧；若本连接还没发过首帧，则这次发首帧（连接级认证 +
// 打开该流）。串行化保证并发 Open 时首帧只发一次且排最前。
func (m *MuxConn) writeOrdered(id uint32, host string, port uint16) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if !m.firstSent {
		m.firstSent = true
		return m.writeRaw(proto.FirstFrame(m.password, uint64(time.Now().Unix()), id, host, port))
	}
	return m.writeRaw(proto.OpenFrame(id, host, port))
}

func (m *MuxConn) writeRaw(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errMuxClosed
	}
	return m.ws.WriteMessage(websocket.BinaryMessage, b)
}

func (m *MuxConn) readLoop() {
	for {
		_, data, err := m.ws.ReadMessage()
		if err != nil {
			// 服务端预算回收是计划内的 close(1000,"budget")：与节点故障区分开，
			// 好让上层不记失败、并提前把下一条连接备好。
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Text == "budget" {
				m.recycled.Store(true)
			}
			m.logf("readLoop error: %v", err)
			m.kill(err)
			return
		}
		if len(data) < 4 {
			continue
		}
		if proto.IsControl(data) {
			if id := proto.ParseCloseControl(data); id != 0 {
				m.mu.Lock()
				s := m.streams[id]
				m.mu.Unlock()
				if s != nil {
					s.remoteClose()
				}
			}
			continue
		}
		id := proto.StreamID(data)
		m.mu.Lock()
		s := m.streams[id]
		m.mu.Unlock()
		if s == nil {
			continue // 已关流的迟到帧，静默丢弃（与 devserver 语义一致）
		}
		if !s.responded.Swap(true) {
			// 等待中的流：第一帧是响应帧 ID(4)|STATUS(1)
			status := data[4]
			if status == proto.StatusOK {
				s.signalReady(nil)
			} else {
				s.signalReady(statusError(status))
			}
			continue
		}
		s.deliver(data[4:])
	}
}

// statusError 把响应帧的 STATUS 翻译成可读错误（英文，客户端输出规范）。
func statusError(status byte) error {
	switch status {
	case proto.StatusBad:
		return errors.New("server rejected stream: bad request or auth (0x01)")
	case proto.StatusForbid:
		return errors.New("server rejected stream: target forbidden (0x02)")
	case proto.StatusNoExit:
		return errors.New("server rejected stream: all exits failed (0x03)")
	default:
		return fmt.Errorf("server rejected stream: status 0x%02x", status)
	}
}

// kill 宣告整条连接死亡：所有流立即关闭，上层重连由 selector 处理
// （PRD：不做会话恢复，在途流直接关闭）。
func (m *MuxConn) kill(cause error) {
	if !m.alive.Swap(false) {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	list := make([]*MuxStream, 0, len(m.streams))
	for _, s := range m.streams {
		list = append(list, s)
	}
	m.streams = make(map[uint32]*MuxStream)
	m.mu.Unlock()
	for _, s := range list {
		s.markDead(cause)
	}
	_ = m.ws.Close()
	if m.onDead != nil {
		go m.onDead(m.recycled.Load())
	}
}

// Close 关闭整条 WS。
func (m *MuxConn) Close() error {
	m.kill(errors.New("mux: closed by caller"))
	return nil
}

// ---- MuxStream：net.Conn ----

type MuxStream struct {
	mux   *MuxConn
	id    uint32
	ready chan error
	once  sync.Once

	responded atomic.Bool // 已收到响应帧
	dead      atomic.Bool

	rmu     sync.Mutex
	rbuf    []byte
	rcond   *sync.Cond
	errOnce sync.Once
	err     error

	// tmu 守 timer：SetDeadline(零值)必须真的撤销上一个计时器。少了这一步，
	// 每次"设 3 秒 deadline 探首字节、探完清除"都会在 3 秒后把这条流杀掉
	// ——浏览器复用隧道拉图片/脚本时必现（实测复杂页面时好时坏、curl 短请求
	// 正常，根因就在这里）。
	tmu   sync.Mutex
	timer *time.Timer
	fired bool // 计时器已触发过：此后的清零/重设都不再复活这条流

	wmu sync.Mutex
}

func newMuxStream(m *MuxConn, id uint32) *MuxStream {
	s := &MuxStream{mux: m, id: id, ready: make(chan error, 1)}
	s.rcond = sync.NewCond(&s.rmu)
	return s
}

func (s *MuxStream) signalReady(err error) {
	s.once.Do(func() { s.ready <- err })
}

func (s *MuxStream) deliver(payload []byte) {
	s.rmu.Lock()
	s.rbuf = append(s.rbuf, payload...)
	s.rmu.Unlock()
	s.rcond.Broadcast()
}

func (s *MuxStream) remoteClose() {
	s.setErr(io.EOF)
	s.markDead(nil)
}

func (s *MuxStream) markDead(cause error) {
	// dead 必须置位：Read 的循环条件依赖它。只 broadcast 不置位的话，
	// Wait 醒来发现 rbuf 仍空、dead 仍 false，会继续睡回去 —— 整个流挂死。
	s.dead.Store(true)
	s.setErr(cause)
	s.rcond.Broadcast()
	s.mux.remove(s.id)
}

func (s *MuxStream) setErr(err error) {
	if err == nil {
		err = io.EOF
	}
	s.errOnce.Do(func() { s.err = err })
}

func (s *MuxStream) Read(b []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.rbuf) == 0 {
		if s.dead.Load() {
			if s.err != nil {
				return 0, s.err
			}
			return 0, io.EOF
		}
		s.rcond.Wait()
	}
	n := copy(b, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

func (s *MuxStream) Write(b []byte) (int, error) {
	if s.dead.Load() {
		return 0, errors.New("mux: stream closed")
	}
	if len(b) > proto.MaxPayload {
		return 0, fmt.Errorf("mux: payload %d exceeds %d", len(b), proto.MaxPayload)
	}
	if err := s.mux.writeRaw(proto.DataFrame(s.id, b)); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (s *MuxStream) Close() error {
	if s.dead.Swap(true) {
		return nil
	}
	if s.responded.Load() {
		// 只在流已建立时发 CLOSE；建立失败时服务端本就没开流。
		_ = s.mux.writeRaw(proto.CloseControl(s.id))
	}
	s.mux.remove(s.id)
	s.rcond.Broadcast()
	return nil
}

func (s *MuxStream) LocalAddr() net.Addr  { return s.mux.ws.LocalAddr() }
func (s *MuxStream) RemoteAddr() net.Addr { return s.mux.ws.RemoteAddr() }

// SetReadDeadline / SetDeadline 作用于单流（timer 关流），绝不作用于共享 WS
// —— 一条流的超时不能拆掉复用同一条连接的其他流。
func (s *MuxStream) SetReadDeadline(t time.Time) error { return s.armTimer(t) }
func (s *MuxStream) SetDeadline(t time.Time) error     { return s.armTimer(t) }
func (s *MuxStream) SetWriteDeadline(t time.Time) error {
	return nil // 写走共享 WS，不做流级门控
}

func (s *MuxStream) armTimer(t time.Time) error {
	s.tmu.Lock()
	if s.timer != nil {
		s.timer.Stop() // 撤销上一个计时器：清零必须真的取消，否则它在原定时间照样开枪
		s.timer = nil
	}
	fired := s.fired
	s.tmu.Unlock()

	if t.IsZero() {
		return nil // deadline 已清除
	}
	if fired || s.dead.Load() {
		return nil // 流已因超时结束：不再复活
	}
	d := time.Until(t)
	if d <= 0 {
		s.markDead(errors.New("mux: deadline exceeded"))
		return nil
	}
	s.tmu.Lock()
	if s.fired || s.timer != nil {
		s.tmu.Unlock()
		return nil
	}
	s.timer = time.AfterFunc(d, func() {
		s.tmu.Lock()
		if s.fired {
			s.tmu.Unlock()
			return
		}
		s.fired = true
		s.tmu.Unlock()
		if !s.dead.Load() {
			s.markDead(errors.New("mux: i/o timeout"))
		}
	})
	s.tmu.Unlock()
	return nil
}
