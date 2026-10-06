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
//
// CREATE_BREAKAWAY_FROM_JOB 是这里的命门：从 Windows Terminal / VSCode 等终端里
// 启动时，终端把整棵进程树放进 Job Object（关窗即全杀，KILL_ON_JOB_CLOSE）——
// DETACHED_PROCESS 只解决"控制台事件连坐"，防不了 Job 连坐。实测现象就是：
// 关掉终端窗口，serve 和看门狗一起蒸发，系统代理悬死在已死端口上。
// 带 breakaway 标志创建若失败（Job 不允许脱离），退回不带标志重试 —— 至少
// 和旧行为一样，不会更糟。
func spawnWatchdog(logger *log.Logger) {
	exe, err := os.Executable()
	if err != nil {
		logger.Printf("WARN locate exe for watchdog: %v", err)
		return
	}
	attrs := []uint32{0x01000000 | 0x00000008 | 0x00000200, 0x00000008 | 0x00000200} // 先试脱离 Job，失败退回
	for _, flags := range attrs {
		cmd := exec.Command(exe, "watchdog")
		cmd.Env = append(os.Environ(), "NETMASTER_OWNER_PID="+fmt.Sprint(os.Getpid()))
		// 脱离：不让看门狗继承控制台输入，独立存活。
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: flags, // CREATE_BREAKAWAY_FROM_JOB | DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		if err := cmd.Start(); err != nil {
			continue // Job 不允许 breakaway 等：换下一组标志
		}
		// 不 Wait，让它独立运行；父进程退出后由它接管清理。
		go func() { _ = cmd.Process.Release() }()
		logger.Printf("[sys] watchdog started (pid %d) to auto-restore on crash", cmd.Process.Pid)
		return
	}
	logger.Printf("WARN start watchdog: all creation flag combinations failed")
}
