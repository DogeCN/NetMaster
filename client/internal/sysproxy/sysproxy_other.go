//go:build !windows && !darwin && !linux

package sysproxy

import "errors"

// Enable 不支持系统代理的平台：BSD、wasi 等。用 --manual。
func Enable(proxyAddr string) (func(), error) {
	return nil, errors.New("sysproxy: system proxy takeover not supported on this platform (use --manual)")
}

// Disable 同上。
func Disable() error {
	return errors.New("sysproxy: system proxy takeover not supported on this platform")
}

// Restore 同上。
func Restore() error {
	return errors.New("sysproxy: system proxy takeover not supported on this platform")
}

// CleanupStale 无状态文件，无事可做。
func CleanupStale() (bool, error) { return false, nil }

// StatePath 无状态文件。
func StatePath() string { return "" }
