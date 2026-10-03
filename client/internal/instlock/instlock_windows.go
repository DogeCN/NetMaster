//go:build windows

package instlock

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows 没有 flock，用 O_EXCL 建锁文件 + PID 活性探测：锁文件存在但
// 持有进程已死（上次强杀/断电没来得及清理）视为陈旧锁，清掉重拿。
func Acquire(path string) (func(), int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, 0, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		pid := os.Getpid()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			b, _ := json.Marshal(map[string]int{"pid": pid})
			_, _ = f.Write(b)
			f.Close()
			return func() { os.Remove(path) }, pid, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, 0, err
		}
		holder := readPID(path)
		if holder > 0 && processAlive(holder) {
			return nil, holder, ErrLocked
		}
		// 陈旧锁或读不出 PID：删掉重来一次。删完仍被抢（极小竞态）就走下一轮
		// 的 O_EXCL 失败分支报 ErrLocked。
		_ = os.Remove(path)
	}
	return nil, readPID(path), ErrLocked
}

// processAlive 探测进程是否存活。OpenProcess 失败时区分"不存在"与
// "无权访问"——后者（ERROR_ACCESS_DENIED）说明进程在，只是不是本用户的。
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == nil {
		_ = windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
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
