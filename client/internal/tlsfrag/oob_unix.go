//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris

package tlsfrag

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// sendOOBRaw 通过 send(MSG_OOB) 把 data+oob 一次性发出。
func sendOOBRaw(rc syscall.RawConn, data []byte, oob byte) error {
	toSend := make([]byte, len(data)+1)
	copy(toSend, data)
	toSend[len(data)] = oob

	var innerErr error
	ctrlErr := rc.Write(func(fd uintptr) bool {
		innerErr = unix.Send(int(fd), toSend, unix.MSG_OOB)
		return innerErr != unix.EAGAIN
	})
	if ctrlErr != nil {
		return ctrlErr
	}
	return innerErr
}
