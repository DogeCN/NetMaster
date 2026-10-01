//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
)

// spawnWatchdog 非 Windows 平台：系统代理接管本身是 Windows 注册表逻辑，
// 这里仅保留可编译的最小实现（无隐藏窗口/脱离标志的需求）。
func spawnWatchdog(logger *log.Logger) {
	exe, err := os.Executable()
	if err != nil {
		logger.Printf("WARN locate exe for watchdog: %v", err)
		return
	}
	cmd := exec.Command(exe, "watchdog")
	cmd.Env = append(os.Environ(), "NETMASTER_OWNER_PID="+fmt.Sprint(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logger.Printf("WARN start watchdog: %v", err)
		return
	}
	go func() { _ = cmd.Process.Release() }()
	logger.Printf("[sys] watchdog started (pid %d) to auto-restore on crash", cmd.Process.Pid)
}
