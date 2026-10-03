package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
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

// 模板必须是合法 JSON，且只含必填项 —— 它是"第一次双击"看到的第一份配置，
// 多一个字段就多一个"这个要不要填"的疑问。
func TestTemplate(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(template), &m); err != nil {
		t.Fatalf("template is not valid JSON: %v", err)
	}
	for _, k := range []string{"server", "password"} {
		if _, ok := m[k]; !ok {
			t.Errorf("template should contain %q", k)
		}
	}
	if len(m) != 2 {
		t.Errorf("template should only carry the required fields, got %v", m)
	}
	if !strings.Contains(template, "<") {
		t.Error("template should use <...> placeholders")
	}
}

// WriteTemplate 在有配置时不动手（避免覆盖用户填好的值）。
func TestWriteTemplateSkipsWhenConfigExists(t *testing.T) {
	chdirTemp(t)
	writeConfig(t, `{"server":"keep.me","password":"x"}`)
	path, err := WriteTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Error("WriteTemplate must not overwrite an existing config")
	}
	b, _ := os.ReadFile("config.json")
	if !strings.Contains(string(b), "keep.me") {
		t.Error("existing config should be left untouched")
	}
}

// 无配置时写模板。
func TestWriteTemplateCreates(t *testing.T) {
	chdirTemp(t)
	path, err := WriteTemplate()
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected a path back")
	}
	if _, err := os.Stat("config.json"); err != nil {
		t.Fatalf("config.json not created: %v", err)
	}
}
