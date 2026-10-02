//go:build linux

package sysproxy

// Linux 系统代理（PRD §3.1 可选增强）。走 gsettings 写 GNOME/KDE 常见的
// org.gnome.system.proxy.* —— 覆盖绝大多数桌面环境（GNOME、Unity、Cinnamon、
// KDE Plasma 的 gsettings 后端）。Xfce/MATE 等用别的 schema，最诚实的做法是
// 明确告诉用户改用 --manual，而不是留一个半生效的设置。
//
// 同 macOS：只接管 HTTP/HTTPS，不设 SOCKS。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var stateFile = filepath.Join(os.TempDir(), "netmaster_sysproxy_linux.json")

type gsettingsKeys struct {
	schema    string
	mode      string // "http" | "https"
	onKey     string
	hostKey   string
	portKey   string
	socksHost string
	socksPort string
}

var gsettingsTargets = []gsettingsKeys{
	{"org.gnome.system.proxy.http", "http", "org.gnome.system.proxy.http enable", "org.gnome.system.proxy.http host", "org.gnome.system.proxy.http port", "org.gnome.system.proxy.http socks-host", "org.gnome.system.proxy.http socks-port"},
	{"org.gnome.system.proxy.https", "https", "org.gnome.system.proxy.https enable", "org.gnome.system.proxy.https host", "org.gnome.system.proxy.https port", "org.gnome.system.proxy.https socks-host", "org.gnome.system.proxy.https socks-port"},
	{"org.gnome.system.proxy", "global", "org.gnome.system.proxy mode", "", "", "", ""},
}

type linuxState struct {
	ProxyAddr string `json:"proxy_addr"`
}

// gsettings 可用性检查：没有 gsettings 命令就不是 GNOME 系，直接拒绝。
func gsettingsAvailable() error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return fmt.Errorf("sysproxy: gsettings not found (non-GNOME desktop? use --manual)")
	}
	return nil
}

func gset(key, value string) error {
	cmd := exec.Command("gsettings", "set", key, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gsettings set %s=%s: %v (%s)", key, value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gget(key string) (string, error) {
	out, err := exec.Command("gsettings", "get", key).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Trim(string(out), "'")), nil
}

// Enable 打开 HTTP/HTTPS 系统代理。
func Enable(proxyAddr string) (func(), error) {
	if err := gsettingsAvailable(); err != nil {
		return nil, err
	}
	host, portStr, err := splitAddr(proxyAddr)
	if err != nil {
		return nil, err
	}
	for _, t := range gsettingsTargets {
		if t.mode == "global" {
			if err := gset(t.onKey, "'manual'"); err != nil {
				return nil, err
			}
			continue
		}
		if err := gset(t.onKey, "true"); err != nil {
			return nil, err
		}
		if err := gset(t.hostKey, "'"+host+"'"); err != nil {
			return nil, err
		}
		if err := gset(t.portKey, portStr); err != nil {
			return nil, err
		}
	}
	b, _ := json.Marshal(linuxState{ProxyAddr: proxyAddr})
	_ = os.WriteFile(stateFile, b, 0o600)
	return nil, nil
}

// Disable 关掉 HTTP/HTTPS 并把 proxy mode 置为 none。
func Disable() error {
	for _, t := range gsettingsTargets {
		if t.mode == "global" {
			if err := gset(t.onKey, "'none'"); err != nil {
				return err
			}
			continue
		}
		if err := gset(t.onKey, "false"); err != nil {
			return err
		}
	}
	return nil
}

// Restore 关掉系统代理并清掉状态文件。
func Restore() error {
	if err := gsettingsAvailable(); err != nil {
		return err
	}
	if err := Disable(); err != nil {
		return err
	}
	_ = os.Remove(stateFile)
	return nil
}

// CleanupStale 上次异常退出残留 = proxy mode 还是 manual。
func CleanupStale() (bool, error) {
	if _, err := os.Stat(stateFile); err != nil {
		return false, nil
	}
	if err := gsettingsAvailable(); err != nil {
		return false, nil
	}
	mode, err := gget("org.gnome.system.proxy mode")
	if err != nil {
		return false, err
	}
	if mode == "manual" {
		return true, Restore()
	}
	return false, nil
}

// StatePath 返回状态文件路径。
func StatePath() string { return stateFile }

// splitAddr 拆 "host:port"。
func splitAddr(addr string) (string, string, error) {
	host, port, ok := strings.Cut(addr, ":")
	if !ok || host == "" || port == "" {
		return "", "", fmt.Errorf("sysproxy: bad proxy address %q", addr)
	}
	return host, port, nil
}
