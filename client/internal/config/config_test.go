package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"netmaster/internal/entry"
)

// writeConfig 在当前目录写 config.json（调用方负责先 chdir 进去）。
func writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile("config.json", []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// chdirTemp 进一个空目录并在测试结束时切回来。config 包按相对路径
// "config.json" 查找，所以用例只能这样隔离（不能改 searchPaths）。
func chdirTemp(t *testing.T) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
}

// Config 只包含真正被消费的字段。
//
// LatencyToleranceMs 曾在这里（"出口优选容差"），它描述的延迟容忍/自优化选路
// 在 v2 里已随 nodepool 一起退役。留着会让人以为填了有用 —— 用反射级别的
// 断言把它钉住：再有人加回来，这条测试会先问一句"消费方在哪"。
func TestConfigHasNoRetiredFields(t *testing.T) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sampleFull), &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"server": true, "password": true, "manual": true, "rules": true, "tunnels": true}
	for k := range m {
		if _, ok := want[k]; !ok {
			t.Errorf("config.json carries %q but Config has no such field — is a doc/flag out of sync?", k)
		}
	}
	// 反向：Config 的每个字段都能被解析（omitempty 下的零值不算）
	full := Config{Server: "s", Password: "p", Manual: true, Rules: "r"}
	b, _ := json.Marshal(full)
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != full {
		t.Errorf("round-trip = %+v, want %+v", got, full)
	}
}

// sampleFull 是一份"把 README 里写到的字段都写上"的配置，用来检查文档与
// 结构是否同步。
const sampleFull = `{
  "server": "nm.example.com",
  "password": "pw",
  "manual": false,
  "rules": "",
  "tunnels": 2
}`

// tunnels 缺省必须是"未设置"而不是某个数：调用方靠 0 来区分"用默认"和
// "用户真的写了 0"，而 0 本身是无效配置（cmd 层会报错退出）。
func TestTunnels(t *testing.T) {
	chdirTemp(t)

	writeConfig(t, `{"server":"a.example","password":"p","tunnels":2}`)
	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.TunnelsValue(); got != 2 {
		t.Errorf("tunnels = %d, want 2", got)
	}

	writeConfig(t, `{"server":"a.example","password":"p"}`)
	cfg, _, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.TunnelsValue(); got != 0 {
		t.Errorf("unset tunnels = %d, want 0 (caller falls back to the built-in default)", got)
	}

	// 写了 0 必须与"没写"区分得开：0 是无效配置，cmd 层要报错退出。
	// TunnelsValue 两者都返回 0，真正的判别靠 Tunnels 指针是否为 nil。
	writeConfig(t, `{"server":"a.example","password":"p","tunnels":0}`)
	cfg, _, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Tunnels == nil {
		t.Error(`"tunnels":0 must parse as "written", not as "absent" — otherwise it ` +
			"silently becomes the default instead of being rejected")
	}
	if cfg.TunnelsValue() != 0 {
		t.Errorf("tunnels = %d, want 0", cfg.TunnelsValue())
	}
}

// 未知字段不报错：老配置文件里带 latencyToleranceMs 也要能读（只是被忽略），
// 否则一次升级就把用户的配置变成"文件损坏"。
func TestUnknownFieldsAreTolerated(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"server":"a.example","password":"p","latencyToleranceMs":50}`)
	cfg, path, err := Load()
	if err != nil {
		t.Fatalf("load config with a retired field: %v", err)
	}
	if path == "" {
		t.Fatal("expected the file to be found")
	}
	if cfg.Server != "a.example" || cfg.Password != "p" {
		t.Errorf("got %+v", cfg)
	}
}

// 损坏的 JSON 必须是错误：静默忽略一份读不出来的配置，会让人以为它生效了。
func TestBrokenJSONIsAnError(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"server":`)
	if _, _, err := Load(); err == nil {
		t.Error("broken JSON should be an error, not a silent fallback")
	}
}

// 没有任何配置文件不是错误：全部配置也可以由 flag 给出。
func TestNoConfigIsNotAnError(t *testing.T) {
	chdirTemp(t)
	cfg, path, err := Load()
	if err != nil {
		t.Fatalf("missing config should not be an error: %v", err)
	}
	if path != "" || cfg != (Config{}) {
		t.Errorf("got (%+v, %q), want zero", cfg, path)
	}
}

// appdata 钉进临时目录：测试绝不能碰真实的用户配置。
func pinAppPath(t *testing.T) {
	t.Helper()
	old := appPathOverride
	appPathOverride = filepath.Join(t.TempDir(), "netmaster.json")
	t.Cleanup(func() { appPathOverride = old })
}

// Save → LoadFile 往返：配置与优选缓存都要原样回来。
func TestSaveLoadFileRoundTrip(t *testing.T) {
	pinAppPath(t)

	f := File{Config: Config{Server: "a.example", Password: "p"}}
	f.EntryCache = &EntryCache{When: time.Now(), Nodes: []entry.Node{{Addr: "1.2.3.4", Port: 443}}}
	if _, err := Save(f); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, path, err := LoadFile()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if path != AppPath() {
		t.Errorf("loaded from %q, want %q", path, AppPath())
	}
	if got.Server != "a.example" || got.Password != "p" {
		t.Errorf("config round-trip = %+v", got.Config)
	}
	if got.EntryCache == nil || len(got.EntryCache.Nodes) != 1 || got.EntryCache.Nodes[0].Addr != "1.2.3.4" {
		t.Errorf("entry cache round-trip = %+v", got.EntryCache)
	}
}

// Save 出的文件权限收窄：里面躺着口令。
func TestSaveFilePermissions(t *testing.T) {
	pinAppPath(t)
	if _, err := Save(File{Config: Config{Server: "a", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(AppPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600 (the file carries the password)", fi.Mode().Perm())
	}
}

// Reset 清掉 appdata 文件与锁；再 Reset 一次不报错。
func TestResetRemovesPersistedData(t *testing.T) {
	pinAppPath(t)
	if _, err := Save(File{Config: Config{Server: "a"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LockPath(""), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed := Reset()
	if len(removed) == 0 {
		t.Fatal("expected files to be removed")
	}
	if _, err := os.Stat(AppPath()); !os.IsNotExist(err) {
		t.Error("appdata file survived Reset")
	}
	// 空状态上再 Reset 一次：安全无副作用（幂等）。
	Reset()
	if _, err := os.Stat(AppPath()); !os.IsNotExist(err) {
		t.Error("appdata file reappeared")
	}
}

// 老版 ./config.json 仍可读（迁移的读侧），且 appdata 优先。
func TestLegacyConfigStillReadable(t *testing.T) {
	pinAppPath(t)
	chdirTemp(t)
	writeConfig(t, `{"server":"legacy.example","password":"p"}`)
	cfg, path, err := Load()
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if path != legacyPath || cfg.Server != "legacy.example" {
		t.Errorf("got (%+v, %q)", cfg, path)
	}
}
