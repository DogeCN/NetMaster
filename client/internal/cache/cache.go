// Package cache 提供进程间共享的磁盘缓存。
//
// 存在的理由：拉订阅和探测节点延迟都是秒级的网络操作，而每次启动、每次
// `test`/`sub` 都要重做一遍。缓存到磁盘后，这些成本只在缓存过期时付一次，
// 而不是每条命令都付。
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Dir 返回缓存目录，不存在时创建。
func Dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	d := filepath.Join(base, "netmaster")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// file 把命名空间与标识串映射成缓存文件名。
//
// 标识串通常是 URL 或节点列表，可能含路径分隔符与换行，所以哈希后再用；
// 命名空间保留前缀，便于人肉排查目录里都是些什么。
func file(namespace, id string) string {
	sum := sha256.Sum256([]byte(id))
	return namespace + "-" + hex.EncodeToString(sum[:8]) + ".json"
}

// entry 是落盘的包装结构，额外记住写入时间以便判断新鲜度。
type entry struct {
	SavedAt time.Time       `json:"savedAt"`
	Data    json.RawMessage `json:"data"`
}

// Load 读出缓存并返回其年龄。ok=false 表示没有可用缓存（不存在、损坏或结构不匹配）。
func Load(namespace, id string, v any) (age time.Duration, ok bool) {
	d, err := Dir()
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(d, file(namespace, id)))
	if err != nil {
		return 0, false
	}
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		return 0, false
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return 0, false
	}
	return time.Since(e.SavedAt), true
}

// Save 原子写入缓存（先写临时文件再改名，避免进程中断留下半截文件）。
func Save(namespace, id string, v any) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b, err := json.Marshal(entry{SavedAt: time.Now(), Data: raw})
	if err != nil {
		return err
	}
	p := filepath.Join(d, file(namespace, id))
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Clear 删除某一项缓存。不存在视为成功。
func Clear(namespace, id string) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(d, file(namespace, id)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ClearAll 清空整个缓存目录里的本程序文件。
func ClearAll() error {
	d, err := Dir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(d, e.Name()))
	}
	return nil
}
