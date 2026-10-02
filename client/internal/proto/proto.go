// Package proto 实现协议 v2 的帧编解码与鉴权派生。
// 权威定义见 server/src/protocol.js（两端互为镜像，字段全部大端）：
//
//	首帧   AUTH(16) | TS(8) | STREAM_ID(4) | ATYP(1) | ADDR | PORT(2)
//	开帧   STREAM_ID(4) | ATYP(1) | ADDR | PORT(2)
//	数据帧 STREAM_ID(4) | PAYLOAD            PAYLOAD ≤ 64 KB
//	控制帧 0x00000000 | CTRL_TYPE(1) | ...    总长 ≤ 9 字节
//	响应帧 STREAM_ID(4) | STATUS(1)
//
// 流 ID ∈ [1, 0xFFFFFFFE]；0 是控制帧专用前缀，0xFFFFFFFF 永久保留；
// 递增到 0xFFFFFFFE 后回绕到 1（会话内不复用已关流的 ID）。
package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
)

const (
	MaxPayload   = 64 * 1024
	StreamIDMax  = 0xFFFFFFFE
	TSWindowSec  = 300
	StatusOK     = 0x00
	StatusBad    = 0x01 // 格式或认证错误
	StatusForbid = 0x02 // 目标禁连
	StatusNoExit = 0x03 // 出口全部失败

	ATypIPv4   = 0x01
	ATypDomain = 0x02
	ATypIPv6   = 0x03

	CtrlClose = 0x01
)

// ValidStreamID 报告 id 是否落在合法区间。
func ValidStreamID(id uint32) bool { return id >= 1 && id <= StreamIDMax }

// Auth 计算 HMAC-SHA256(PASSWORD, TS‖ID‖ATYP‖ADDR‖PORT)[:16]。
// signed 是首帧 TS 起的全部字节（与 server/src/crypto.js 的 authCode 输入一致）。
func Auth(password string, signed []byte) []byte {
	m := hmac.New(sha256.New, []byte(password))
	m.Write(signed)
	return m.Sum(nil)[:16]
}

// EncodeAddr 把 host/port 编为 ATYP|ADDR|PORT。
func EncodeAddr(host string, port uint16) []byte {
	var atyp byte
	var addr []byte
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			atyp, addr = ATypIPv4, ip4
		} else {
			atyp, addr = ATypIPv6, ip.To16()
		}
	} else {
		atyp = ATypDomain
		h := strings.ToLower(host)
		addr = make([]byte, 0, 1+len(h))
		addr = append(addr, byte(len(h)))
		addr = append(addr, h...)
	}
	out := make([]byte, 0, 1+len(addr)+2)
	out = append(out, atyp)
	out = append(out, addr...)
	return binary.BigEndian.AppendUint16(out, port)
}

// SplitTarget 把 "host:port" 拆成 host 与 uint16 端口。
func SplitTarget(target string) (string, uint16, error) {
	host, ps, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, err
	}
	p, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		return "", 0, errors.New("proto: invalid port in " + target)
	}
	return host, uint16(p), nil
}

// FirstFrame 构造首帧：AUTH(16)|TS(8)|ID(4)|ADDR。auth 必须覆盖 TS 起的全部字节，
// 因此先拼出签名区再算 HMAC——signRegion 返回 TS 起的字节序列。
func FirstFrame(password string, tsSec uint64, id uint32, host string, port uint16) []byte {
	signed := make([]byte, 0, 8+4+7)
	signed = binary.BigEndian.AppendUint64(signed, tsSec)
	signed = binary.BigEndian.AppendUint32(signed, id)
	signed = append(signed, EncodeAddr(host, port)...)

	out := make([]byte, 0, 16+len(signed))
	out = append(out, Auth(password, signed)...)
	return append(out, signed...)
}

// OpenFrame 构造开帧（流 2+）：ID(4)|ADDR。
func OpenFrame(id uint32, host string, port uint16) []byte {
	out := binary.BigEndian.AppendUint32(nil, id)
	return append(out, EncodeAddr(host, port)...)
}

// DataFrame 构造数据帧：ID(4)|PAYLOAD。调用方保证 len(payload) ≤ MaxPayload。
func DataFrame(id uint32, payload []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, id)
	return append(out, payload...)
}

// Response 构造响应帧：ID(4)|STATUS(1)。
func Response(id uint32, status byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, id)
	return append(out, status)
}

// CloseControl 构造 CLOSE 控制帧：0x00000000|0x01|ID(4)，共 9 字节。
func CloseControl(id uint32) []byte {
	out := []byte{0, 0, 0, 0, CtrlClose}
	return binary.BigEndian.AppendUint32(out, id)
}

// StreamID 读帧头 4 字节流 ID（未做 unsigned 归一，仅服务端帧分发用）。
func StreamID(b []byte) uint32 {
	return binary.BigEndian.Uint32(b)
}

// IsControl 报告帧是否控制帧（前缀 0x00000000，流 ID ≥ 1 的帧不可能出现）。
func IsControl(b []byte) bool {
	return len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 0
}

// ParseCloseControl 解析 CLOSE 控制帧；非 CLOSE 或长度不足返回 0。
func ParseCloseControl(b []byte) uint32 {
	if len(b) < 9 || b[4] != CtrlClose {
		return 0
	}
	return binary.BigEndian.Uint32(b[5:9])
}
