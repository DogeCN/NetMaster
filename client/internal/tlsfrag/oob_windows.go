//go:build windows

package tlsfrag

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// sendOOBRaw 通过 WSASend(MSG_OOB) 把 data+oob 一次性发出。
// Windows 的 Winsock 对 send 上的 MSG_OOB 支持与 BSD 一致：oob 字节作为紧急数据
// 附加在 data 之后。
func sendOOBRaw(rc syscall.RawConn, data []byte, oob byte) error {
	toSend := make([]byte, len(data)+1)
	copy(toSend, data)
	toSend[len(data)] = oob
	wsabuf := windows.WSABuf{Len: uint32(len(toSend)), Buf: &toSend[0]}

	var innerErr error
	ctrlErr := rc.Write(func(fd uintptr) bool {
		var n uint32
		innerErr = windows.WSASend(windows.Handle(fd), &wsabuf, 1, &n, windows.MSG_OOB, nil, nil)
		// EWOULDBLOCK = 发送缓冲满，RawConn 会再调 fn；其余情况（成功或真错误）都算完。
		return innerErr != windows.WSAEWOULDBLOCK
	})
	if ctrlErr != nil {
		return ctrlErr
	}
	if innerErr != nil && innerErr != windows.NOERROR {
		return innerErr
	}
	return nil
}
