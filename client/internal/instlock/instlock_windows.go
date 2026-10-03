//go:build windows

package instlock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows 用命名互斥体：内核在持有进程死亡时自动释放，没有"陈旧锁"，
// 也没有 PID 复用带来的误判（PID 探测会把复用了死进程 PID 的新进程当成
// 持有者，实测踩过）。锁文件只用来给用户展示持有者 PID，不参与互斥。
//
// 作用域说明（别被"全局"两个字误导）：默认名带 `Local\` 前缀，那是**会话作用域**
// —— 同一台机器上、同一个用户的不同登录会话（两个 RDP、控制台 + RDP 等）各自看到
// 自己的命名空间，于是能同时"获取成功"。而系统代理写在 HKCU，是按用户共享的，
// 所以两个会话会真抢。改用 `Global\` 需要 SeCreateGlobalPrivilege（普通用户默认
// 没有），因此这不是一个能顺手改掉的 bug，现状是明确的已知限制：多会话场景下
// 单实例保护不生效。
//
// 测试用 NETMASTER_LOCK_NAME 换独立名字，否则测试会被本机正在运行的 serve 卡死。
func mutexName() string {
	if v := os.Getenv("NETMASTER_LOCK_NAME"); v != "" {
		return `Local\` + v
	}
	return `Local\netmaster-serve`
}

func Acquire(path string) (func(), int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, 0, err
	}
	name, err := syscall.UTF16PtrFromString(mutexName())
	if err != nil {
		return nil, 0, err
	}
	h, err := windows.CreateMutex(nil, true, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// 互斥体已存在 = 已有实例（拿到的句柄指向既有互斥体，关掉即可）
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return nil, readPID(path), ErrLocked
	}
	if err != nil {
		return nil, 0, err
	}

	pid := os.Getpid()
	if f, ferr := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); ferr == nil {
		b, _ := json.Marshal(map[string]int{"pid": pid})
		_, _ = f.Write(b)
		f.Close()
	}
	return func() {
		_ = windows.ReleaseMutex(h)
		_ = windows.CloseHandle(h)
		_ = os.Remove(path)
	}, pid, nil
}

func readPID(path string) int {
	var v struct {
		PID int `json:"pid"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	_ = json.Unmarshal(b, &v)
	// 兼容手工创建的裸 PID 文本文件
	if v.PID == 0 && len(b) > 0 && b[0] != '{' {
		v.PID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	return v.PID
}
