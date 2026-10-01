//go:build windows

package procwait

import "golang.org/x/sys/windows"

// Wait blocks until the process with the given pid exits.
// Used by the watchdog: parent (netmaster serve) dies → watchdog restores system proxy.
func Wait(pid int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// 进程已不存在，视为已退出。
		return
	}
	defer windows.CloseHandle(h) //nolint:errcheck
	windows.WaitForSingleObject(h, windows.INFINITE)
}
