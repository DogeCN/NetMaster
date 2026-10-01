//go:build !windows

package sysproxy

import "errors"

// Enable 非 Windows 平台暂不支持（可用 env / 手动配置系统代理）。
func Enable(proxyAddr string) (func(), error) {
	return nil, errors.New("sysproxy: not supported on this platform")
}

// Disable 非 Windows 平台暂不支持。
func Disable() error {
	return errors.New("sysproxy: not supported on this platform")
}

// Restore 非 Windows 平台暂不支持。
func Restore() error {
	return errors.New("sysproxy: not supported on this platform")
}

// CleanupStale 非 Windows 平台无事可做。
func CleanupStale() (bool, error) { return false, nil }

// StatePath 非 Windows 平台无状态文件。
func StatePath() string { return "" }
