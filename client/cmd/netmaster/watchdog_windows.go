//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
)

// spawnWatchdog 启动一个脱离的看门狗子进程，监视当前进程；当前进程异常消失时由它还原系统代理。
func spawnWatchdog(logger *log.Logger) {
	exe, err := os.Executable()
	if err != nil {
		logger.Printf("WARN locate exe for watchdog: %v", err)
		return
	}
	cmd := exec.Command(exe, "watchdog")
	cmd.Env = append(os.Environ(), "NETMASTER_OWNER_PID="+fmt.Sprint(os.Getpid()))
	// 脱离：不让看门狗继承控制台输入，独立存活。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		logger.Printf("WARN start watchdog: %v", err)
		return
	}
	// 不 Wait，让它独立运行；父进程退出后由它接管清理。
	go func() { _ = cmd.Process.Release() }()
	logger.Printf("[sys] watchdog started (pid %d) to auto-restore on crash", cmd.Process.Pid)
}
