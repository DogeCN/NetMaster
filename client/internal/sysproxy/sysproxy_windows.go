//go:build windows

package sysproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const keyPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// stateFile 记录被覆盖前的旧系统代理配置，用于：
//  1. 正常退出时恢复；
//  2. 进程被强杀/崩溃后，下次启动能自愈把残留清掉。
var stateFile = filepath.Join(os.TempDir(), "netmaster_sysproxy.json")

type savedState struct {
	ProxyEnable    uint32 `json:"proxy_enable"`
	ProxyServer    string `json:"proxy_server"`
	ProxyOverride  string `json:"proxy_override"`
	HadProxyEnable bool   `json:"had_proxy_enable"`
	HadServer      bool   `json:"had_server"`
	HadOverride    bool   `json:"had_override"`
}

func openKey() (registry.Key, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return k, fmt.Errorf("open IE settings (HKCU): %w", err)
	}
	return k, nil
}

// notifyChange 通知系统代理设置已变更，让 Chrome/Edge/FireFox 等立刻刷新。
// 用 wininet 的 InternetSetOptionW(NULL, INTERNET_OPTION_SETTINGS_CHANGED/REFRESH)。
var (
	wininet            = syscall.NewLazyDLL("wininet.dll")
	procInternetSetOpt = wininet.NewProc("InternetSetOptionW")
)

const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

func notifyChange() {
	// 广播 WM_SETTINGCHANGE（"Internet Settings"），WinINET/浏览器据此重载。
	notifySettingChange()
	// 再让 WinINET 主动刷新已缓存的连接设置。
	procInternetSetOpt.Call(0, internetOptionSettingsChanged, 0, 0) //nolint:errcheck
	procInternetSetOpt.Call(0, internetOptionRefresh, 0, 0)         //nolint:errcheck
}

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

const (
	wmSettingChange = 0x1A // WM_SETTINGCHANGE
	hwndBroadcast   = 0xffff
	smfpAbortIfHung = 0x0002
)

func notifySettingChange() {
	regKey, err := windows.UTF16PtrFromString("Internet Settings")
	if err != nil {
		return
	}
	// SendMessageTimeoutW(HWND_BROADCAST, WM_SETTINGCHANGE, 0, "Internet Settings", SMTO_ABORTIFHUNG, 1000, nil)
	//nolint:errcheck
	procSendMessageTimeoutW.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(regKey)),
		uintptr(smfpAbortIfHung),
		1000, // 1s 超时，避免卡住
		0,
	)
}

// Enable 打开系统代理，proxyAddr 形如 127.0.0.1:8080。
// 写 HKCU 不需要管理员。会先把旧配置存到状态文件，再覆盖；返回恢复函数。
func Enable(proxyAddr string) (func(), error) {
	k, err := openKey()
	if err != nil {
		return nil, err
	}
	defer k.Close()

	// 保存旧值（记录是否原本存在）
	st := savedState{}
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil {
		st.ProxyEnable, st.HadProxyEnable = uint32(v), true
	}
	if v, _, err := k.GetStringValue("ProxyServer"); err == nil {
		st.ProxyServer, st.HadServer = v, true
	}
	if v, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		st.ProxyOverride, st.HadOverride = v, true
	}
	// 落盘，供异常退出后的自愈
	if b, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(stateFile, b, 0600)
	}

	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return nil, err
	}
	if err := k.SetStringValue("ProxyServer", proxyAddr); err != nil {
		return nil, err
	}
	// 绕过本地/局域网，避免回环
	if err := k.SetStringValue("ProxyOverride", "localhost;127.*;<local>"); err != nil {
		return nil, err
	}
	notifyChange()

	// 恢复：重新打开 key（原来的已随 defer 关闭），按状态文件恢复。
	restore := func() {
		_ = Restore()
	}
	return restore, nil
}

// Disable 关闭系统代理（恢复到启用前状态，若无状态文件则直接关）。
func Disable() error {
	return Restore()
}

// Restore 把系统代理恢复到上次 Enable 之前的状态，并删除状态文件。
// 若状态文件不存在（从未由 netmaster 设置），退化为关闭代理。
func Restore() error {
	st, hadState := loadState()

	k, err := openKey()
	if err != nil {
		return err
	}
	defer k.Close()

	if !hadState {
		// 没有历史记录：保守起见只关掉代理开关
		if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
			return err
		}
		notifyChange()
		return nil
	}

	if st.HadProxyEnable {
		_ = k.SetDWordValue("ProxyEnable", st.ProxyEnable)
	} else {
		_ = k.DeleteValue("ProxyEnable")
	}
	if st.HadServer {
		_ = k.SetStringValue("ProxyServer", st.ProxyServer)
	} else {
		_ = k.DeleteValue("ProxyServer")
	}
	if st.HadOverride {
		_ = k.SetStringValue("ProxyOverride", st.ProxyOverride)
	} else {
		_ = k.DeleteValue("ProxyOverride")
	}
	_ = os.Remove(stateFile)
	notifyChange()
	return nil
}

// CleanupStale 在启动时调用：若存在状态文件（说明上次异常退出，代理仍指向 netmaster），
// 先恢复干净，避免系统代理指向已不存在的端口。
func CleanupStale() (bool, error) {
	if _, err := os.Stat(stateFile); err != nil {
		return false, nil // 无状态文件，正常
	}
	if err := Restore(); err != nil {
		return true, err
	}
	return true, nil
}

func loadState() (savedState, bool) {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return savedState{}, false
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil {
		return savedState{}, false
	}
	return st, true
}

// StatePath 返回状态文件路径（供诊断/测试）。
func StatePath() string { return stateFile }
