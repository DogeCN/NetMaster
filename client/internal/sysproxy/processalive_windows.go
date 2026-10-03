//go:build windows

package sysproxy

import (
	"errors"

	"golang.org/x/sys/windows"
)

// processAlive 探测进程是否存活。OpenProcess 失败时区分"不存在"与
// "无权访问"——后者说明进程在，只是不是本用户的。
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == nil {
		_ = windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
