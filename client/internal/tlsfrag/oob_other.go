//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || windows)

package tlsfrag

import "syscall"

// sendOOBRaw 在其余平台上不可用；调用方收到 errOOBUnsupported 后退回普通分片。
func sendOOBRaw(rc syscall.RawConn, data []byte, oob byte) error {
	return errOOBUnsupported
}
