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
		// 只关 fd，不删文件。
		//
		// 删文件会开一个经典的 unlink-then-flock 竞态：flock 随 f.Close() 当场释放，
		// 而此刻路径还链着旧 inode —— 另一个进程能在"释放"与"删除"之间拿到同一个
		// inode 并抢到锁，接着我们的 os.Remove 把这个锁的路径删掉；第三个进程再
		// OpenFile 时路径已不存在，会新建一个 inode，也拿到锁。于是两个实例同时
		// 认为自己持有单实例锁，一起抢系统代理（实测症状：运行中的 serve 底下
		// ProxyEnable 被强制置 0）。
		//
		// flock 的语义决定了残留文件是无害的：进程一死内核就放锁，文件本身不是锁。
		// readPID 读出来的过期 pid 只用于错误提示文案，不参与互斥判断。
		f.Close()
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
