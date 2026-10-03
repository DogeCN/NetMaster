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
const mutexName = `Local\netmaster-serve`

func Acquire(path string) (func(), int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, 0, err
	}
	name, err := syscall.UTF16PtrFromString(mutexName)
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
