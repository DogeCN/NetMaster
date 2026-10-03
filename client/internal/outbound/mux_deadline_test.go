package outbound

import (
	"testing"
	"time"
)

// TestArmTimerClearCancels 钉死一个真实故障：SetReadDeadline 设了窗口、探完用
// 零值清除后，旧计时器仍在原定时间把流杀掉。proxy 的 relayWithReplay /
// relayWithProxyReplay 正是这个用法（3 秒探首字节 → 清除 → 转入长期转发），
// 症状是浏览器复杂页面时好时坏而 curl 短请求正常。
func TestArmTimerClearCancels(t *testing.T) {
	m := &MuxConn{streams: map[uint32]*MuxStream{}}
	s := newMuxStream(m, 1)
	m.streams[1] = s

	_ = s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_ = s.SetReadDeadline(time.Time{}) // 清除

	time.Sleep(400 * time.Millisecond)
	if s.dead.Load() {
		t.Fatal("stream died after the deadline was cleared — the old timer still fired")
	}
}

// TestArmTimerFires 未清除时计时器照常生效（别把上一个修复写成"永不超时"）。
func TestArmTimerFires(t *testing.T) {
	m := &MuxConn{streams: map[uint32]*MuxStream{}}
	s := newMuxStream(m, 1)
	m.streams[1] = s

	_ = s.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	time.Sleep(350 * time.Millisecond)
	if !s.dead.Load() {
		t.Fatal("stream survived past an uncleared deadline")
	}
	if _, err := s.Read(make([]byte, 1)); err == nil || err.Error() != "mux: i/o timeout" {
		t.Fatalf("read err = %v, want mux: i/o timeout", err)
	}
}

// TestArmTimerRearmAfterFire 计时器开过枪之后，后续设 deadline 不再复活死流。
func TestArmTimerRearmAfterFire(t *testing.T) {
	m := &MuxConn{streams: map[uint32]*MuxStream{}}
	s := newMuxStream(m, 1)
	m.streams[1] = s

	_ = s.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	time.Sleep(250 * time.Millisecond)
	if !s.dead.Load() {
		t.Fatal("precondition: stream should be dead")
	}
	_ = s.SetReadDeadline(time.Now().Add(2 * time.Second))
	if s.dead.Load() != true {
		t.Fatal("stream was revived after the deadline fired")
	}
	// 也不能因为"复活"而在 2 秒后再次动作（死流不该被计时器移出 map 之外的东西影响）
	if _, ok := m.streams[1]; ok {
		t.Fatal("dead stream still registered in mux")
	}
}
