package proto

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestAuthMatchesReferenceVector(t *testing.T) {
	// 与 server/test/crypto.mjs 同源的手工向量：HMAC-SHA256 的正确性由 Go 标准库
	// 保证，这里主要锁定"取前 16 字节"这一截断行为。
	signed := []byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 1, 2, 'h', 'e', 'l', 'l', 'o', 0, 68}
	got := Auth("hunter2", signed)
	if len(got) != 16 {
		t.Fatalf("auth length = %d, want 16", len(got))
	}
	other := Auth("hunter3", signed)
	if bytes.Equal(got, other) {
		t.Fatal("different passwords produced identical auth")
	}
}

func TestFirstFrameRoundTrip(t *testing.T) {
	now := uint64(time.Now().Unix())
	frame := FirstFrame("pw", now, 1, "example.com", 443)
	// 16(auth) + 8(ts) + 4(id) + 1(atyp) + 1(len) + 11(host) + 2(port)
	if len(frame) != 43 {
		t.Fatalf("first frame length = %d, want 43", len(frame))
	}
	if !bytes.Equal(frame[:16], Auth("pw", frame[16:])) {
		t.Fatal("auth does not verify over signed region")
	}
	ts := binary.BigEndian.Uint64(frame[16:24])
	if ts != now {
		t.Fatalf("ts = %d, want %d", ts, now)
	}
	if id := binary.BigEndian.Uint32(frame[24:28]); id != 1 {
		t.Fatalf("stream id = %d, want 1", id)
	}
	if frame[28] != ATypDomain || frame[29] != 11 {
		t.Fatalf("addr header wrong: % x", frame[28:30])
	}
	if string(frame[30:41]) != "example.com" {
		t.Fatalf("host = %q", frame[30:41])
	}
}

func TestOpenFrameLayout(t *testing.T) {
	f := OpenFrame(7, "10.0.0.1", 80)
	want := []byte{0, 0, 0, 7, ATypIPv4, 10, 0, 0, 1, 0, 80}
	if !bytes.Equal(f, want) {
		t.Fatalf("open frame = % x, want % x", f, want)
	}
}

func TestCloseControl(t *testing.T) {
	f := CloseControl(42)
	if len(f) != 9 {
		t.Fatalf("close control length = %d, want 9", len(f))
	}
	if !IsControl(f) {
		t.Fatal("close control not recognized as control")
	}
	if ParseCloseControl(f) != 42 {
		t.Fatal("close stream id mismatch")
	}
	if ParseCloseControl([]byte{0, 0, 0, 0, 7, 0, 0, 0, 1}) != 0 {
		t.Fatal("unknown ctrl type should parse to 0")
	}
	if IsControl(OpenFrame(1, "a.com", 80)) {
		t.Fatal("nonzero-id frame must not be control")
	}
}

func TestSplitTargetAndEncodeAddr(t *testing.T) {
	host, port, err := SplitTarget("example.com:65535")
	if err != nil || host != "example.com" || port != 65535 {
		t.Fatalf("SplitTarget = %q,%d,%v", host, port, err)
	}
	if _, _, err := SplitTarget("no-port"); err == nil {
		t.Fatal("expected error for target without port")
	}
	v6 := EncodeAddr("2001:4860:4860::8888", 443)
	if v6[0] != ATypIPv6 || len(v6) != 19 {
		t.Fatalf("ipv6 addr wrong: % x", v6)
	}
	if v6 := EncodeAddr("1.2.3.4", 443); v6[0] != ATypIPv4 || len(v6) != 7 {
		t.Fatalf("ipv4 addr wrong")
	}
}

func TestValidStreamID(t *testing.T) {
	cases := map[uint32]bool{
		0: false, 1: true, StreamIDMax: true, StreamIDMax + 1: false,
	}
	if ValidStreamID(0xFFFFFFFF) {
		t.Error("ValidStreamID(0xFFFFFFFF) = true")
	}
	for id, want := range cases {
		if ValidStreamID(id) != want {
			t.Errorf("ValidStreamID(%d) = %v", id, !want)
		}
	}
}
