//go:build !windows

package instlock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// flock 的语义正合适：锁属于"打开的文件描述"，进程一死内核就放锁，
// 所以 unix 上不存在"陈旧锁"问题，不需要读 PID 做活性判断。
func Acquire(path string) (func(), int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, 0, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, readPID(path), ErrLocked
	}
	pid := os.Getpid()
	b, _ := json.Marshal(map[string]int{"pid": pid})
	_ = f.Truncate(0)
	_, _ = f.WriteAt(b, 0)
	return func() {
		f.Close()
		os.Remove(path) // 竞态无害：拿不到就当没有
	}, pid, nil
}

func readPID(path string) int {
	var v struct {
		PID int `json:"pid"`
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &v)
	}
	return v.PID
}
