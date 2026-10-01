//go:build !windows

package procwait

import (
	"os"
	"time"
)

// Wait 非 Windows 平台用轮询近似等待进程退出。
func Wait(pid int) {
	for {
		p, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		// signal 0 只做存在性检查；不存在时返回错误。
		if err := p.Signal(os.Signal(nil)); err != nil {
			return
		}
		time.Sleep(time.Second)
	}
}
