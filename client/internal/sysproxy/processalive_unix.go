//go:build !windows

package sysproxy

import (
	"os"
	"syscall"
)

// processAlive 在 unix 上用 signal 0 探测：能投递（哪怕被拒）说明进程在。
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false // unix 上 FindProcess 仅在 pid 非法时报错，但稳一点
	}
	return p.Signal(syscall.Signal(0)) == nil
}
