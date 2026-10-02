//go:build darwin

package sysproxy

// macOS 系统代理（PRD §3.1 可选增强）。走 networksetup 官方 CLI，不碰
// SystemConfiguration 私有 API —— 后者跨版本会碎，而 networksetup 的行为有文档。
//
// 只处理 HTTP/HTTPS 两个服务：SOCKS 不设（系统代理本身不转发 UDP/TCP 到 SOCKS5
// 之外的东西，而且多数应用的 SOCKS 支持要单独勾选，用户预期与实际差距大）。
//
// 状态落盘在 stateFile：serve 异常退出后 `netmaster restore` 或看门狗能还原。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var stateFile = filepath.Join(os.TempDir(), "netmaster_sysproxy_darwin.json")

// netsetupService 把代理地址映射成 networksetup 的服务名。
func netsetupService(kind string) string {
	return "Wi-Fi" // 常见默认；多网卡/有线环境由用户用 --manual 处理
}

type darwinState struct {
	ProxyAddr string          `json:"proxy_addr"`
	Services  map[string]bool `json:"services"` // 曾被我们改过的服务名
}

// listServices 列出启用的网络服务。
func listServices() []string {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil
	}
	var services []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// 首行是说明文字："An asterisk (*) denotes that a network service is disabled."
		if line == "" || strings.HasSuffix(line, ":") || strings.Contains(line, "asterisk") {
			continue
		}
		services = append(services, strings.TrimSuffix(line, " (*)"))
	}
	return services
}

func runNetSetup(args ...string) error {
	cmd := exec.Command("networksetup", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("networksetup %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Enable 打开 HTTP/HTTPS 系统代理。第二个返回值保留给 Windows 的 onRestore 回调
// （本平台没有，用不到）。
func Enable(proxyAddr string) (func(), error) {
	services := listServices()
	if len(services) == 0 {
		return nil, fmt.Errorf("sysproxy: no network service found (use --manual?)")
	}
	state := darwinState{ProxyAddr: proxyAddr, Services: map[string]bool{}}
	for _, svc := range services {
		for _, kind := range []string{"webproxy", "securewebproxy"} {
			if err := runNetSetup("-"+kind, svc, proxyAddr); err != nil {
				return nil, err
			}
		}
		state.Services[svc] = true
	}
	b, _ := json.Marshal(state)
	_ = os.WriteFile(stateFile, b, 0o600)
	return nil, nil
}

// Disable 关闭系统代理（等价于还原：macOS 上"关闭"就是回到不设代理）。
func Disable() error {
	for _, svc := range listServices() {
		for _, kind := range []string{"webproxy", "securewebproxy"} {
			if err := runNetSetup("-"+kind, svc, "off"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restore 关闭系统代理并清掉状态文件。
func Restore() error {
	if err := Disable(); err != nil {
		return err
	}
	_ = os.Remove(stateFile)
	return nil
}

// CleanupStale 上次异常退出留下的状态需要清理；本平台的"残留"就是代理还开着。
func CleanupStale() (bool, error) {
	if _, err := os.Stat(stateFile); err != nil {
		return false, nil
	}
	return true, Restore()
}

// StatePath 返回状态文件路径（看门狗/诊断用）。
func StatePath() string { return stateFile }
