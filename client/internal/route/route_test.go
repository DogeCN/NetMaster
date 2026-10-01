package route

import (
	"strings"
	"testing"
)

// 默认规则表的实际行为。这是全客户端最关键的一个判断（每个连接的去向都由它定），
// 而在这个文件出现之前，route 包没有任何测试。
func TestDefaultRules(t *testing.T) {
	r, err := LoadDefault(Proxy)
	if err != nil {
		t.Fatalf("load default rules: %v", err)
	}

	cases := []struct {
		host string
		want Action
	}{
		// 强制代理名单：名单内的域名及其子域
		{"google.com", Proxy},
		{"www.google.com", Proxy},
		{"github.com", Proxy},
		{"raw.githubusercontent.com", Proxy},

		// domain 不是裸后缀匹配：notgoogle.com 不该命中 google.com
		{"notgoogle.com", Auto},
		{"google.com.evil.net", Auto},

		// 国内直连
		{"www.baidu.com", Direct},
		{"baidu.com", Direct},
		{"foo.cn", Direct},
		{"a.com.cn", Direct},
		{"12306.cn", Direct},

		// 局域网 CIDR
		{"127.0.0.1", Direct},
		{"192.168.1.1", Direct},
		{"10.1.2.3", Direct},
		{"172.16.5.5", Direct},
		{"localhost", Direct},
		// IPv6 侧也要覆盖：默认规则曾只有 v4 段，于是 [::1]:8080 这种本机服务
		// 会被送去代理（再由边缘去连 ::1，必然失败）。
		{"::1", Direct},
		{"fd00::1", Direct},
		{"fe80::1", Direct},

		// 兜底：未列出的交给出口层按 IP 归属判断
		{"cn.bing.com", Auto},
		{"www.sogou.com", Auto},
		{"example.org", Auto},

		// 公网 IP 不在任何 CIDR 里 → 兜底
		{"93.184.216.34", Auto},
	}

	for _, c := range cases {
		if got := r.Decide(c.host); got != c.want {
			t.Errorf("Decide(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// Decide 收到带端口、大小写、尾部点、IPv6 方括号的主机名时都要归一化。
func TestDecideNormalizesHost(t *testing.T) {
	r, err := LoadDefault(Proxy)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, h := range []string{
		"WWW.Google.Com",
		"www.google.com.",
		"www.google.com:443",
		"  www.google.com  ",
	} {
		if got := r.Decide(h); got != Proxy {
			t.Errorf("Decide(%q) = %q, want proxy", h, got)
		}
	}
	// IPv6 字面量带方括号
	if got := r.Decide("[::1]:443"); got != Direct {
		t.Errorf("Decide([::1]:443) = %q, want direct", got)
	}
}

// domain 与 suffix 的差别必须成立：domain 含精确匹配，suffix 只匹配子域。
func TestDomainVsSuffix(t *testing.T) {
	r := New(Proxy)
	if err := r.Add(Rule{Action: Direct, Kind: KindDomain, Value: "example.com"}); err != nil {
		t.Fatalf("add domain: %v", err)
	}
	if err := r.Add(Rule{Action: Direct, Kind: KindSuffix, Value: "corp.internal"}); err != nil {
		t.Fatalf("add suffix: %v", err)
	}

	// domain：精确 + 子域
	for _, h := range []string{"example.com", "a.example.com", "a.b.example.com"} {
		if got := r.Decide(h); got != Direct {
			t.Errorf("domain: Decide(%q) = %q, want direct", h, got)
		}
	}
	// suffix：只匹配子域，裸的 corp.internal 不匹配
	if got := r.Decide("host.corp.internal"); got != Direct {
		t.Errorf("suffix: Decide(host.corp.internal) = %q, want direct", got)
	}
	if got := r.Decide("corp.internal"); got != Proxy {
		t.Errorf("suffix should not match the bare value, got %q", got)
	}
	// 两者都不做裸子串匹配
	if got := r.Decide("notexample.com"); got != Proxy {
		t.Errorf("Decide(notexample.com) = %q, want proxy (no substring match)", got)
	}
}

// 首条命中即返回，后面的规则不再参与。
func TestFirstMatchWins(t *testing.T) {
	r := New(Proxy)
	if err := r.Add(Rule{Action: Direct, Kind: KindDomain, Value: "a.com"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Rule{Action: Proxy, Kind: KindSuffix, Value: "a.com"}); err != nil {
		t.Fatal(err)
	}
	if got := r.Decide("x.a.com"); got != Direct {
		t.Errorf("first rule should win: got %q, want direct", got)
	}
}

// 已删除的匹配类型必须被明确拒绝，且错误信息要说明现在支持什么 ——
// 静默忽略一条用户写的规则，比拒绝它更糟。
func TestRemovedKindsAreRejected(t *testing.T) {
	for _, kind := range []Kind{"full", "keyword", "regexp", "regex", "bogus"} {
		r := New(Proxy)
		err := r.Add(Rule{Action: Proxy, Kind: kind, Value: "x.com"})
		if err == nil {
			t.Errorf("kind %q should be rejected", kind)
			continue
		}
		if !strings.Contains(err.Error(), "supported:") {
			t.Errorf("kind %q: error should name the supported set, got %q", kind, err)
		}
	}
}

// reject 动作已删除；auto 只能作兜底。
func TestActions(t *testing.T) {
	r := New(Proxy)
	if err := r.Add(Rule{Action: "reject", Kind: KindDomain, Value: "x.com"}); err == nil {
		t.Error("the removed reject action should be rejected")
	}

	err := r.Add(Rule{Action: Auto, Kind: KindDomain, Value: "x.com"})
	if err == nil {
		t.Fatal("auto as a rule action should be rejected")
	}
	if !strings.Contains(err.Error(), "default auto") {
		t.Errorf("error should point at `default auto`, got %q", err)
	}
}

// default 动作必须校验。
//
// 先前 parse 把 `default` 后面的词直接收下不检查，于是 `default proxyy` 会安静地
// 存进去：Decide 返回 "proxyy"，下游 switch 落到 default 分支当成代理 ——
// 规则文件写错了，行为却照常，只是和你以为的不一样。
func TestParseValidatesDefault(t *testing.T) {
	for _, src := range []string{
		"default proxyy\n",
		"default\n",
		"default direct\nproxy lunatic x.com\n",
	} {
		r := New(Proxy)
		if err := r.parse(strings.NewReader(src)); err == nil {
			t.Errorf("parse(%q) should have failed", src)
		}
	}

	r := New(Proxy)
	if err := r.parse(strings.NewReader("default direct\n")); err != nil {
		t.Errorf("valid default should parse: %v", err)
	}
	if got := r.Decide("unlisted.example"); got != Direct {
		t.Errorf("file-level default should take effect, got %q", got)
	}
}

// cidr 只在目标是 IP 时才参与；域名规则不参与 IP 判断，反之亦然。
func TestCIDRAndDomainAreSeparated(t *testing.T) {
	r := New(Proxy)
	if err := r.Add(Rule{Action: Direct, Kind: KindCIDR, Value: "192.168.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Rule{Action: Proxy, Kind: KindDomain, Value: "192.168.1.1"}); err != nil {
		t.Fatal(err)
	}
	// IP 走 cidr 分支，不会被同名 domain 规则抢走
	if got := r.Decide("192.168.1.1"); got != Direct {
		t.Errorf("IP should be decided by cidr: got %q", got)
	}
	// 坏 CIDR 要在 Add 时报错，而不是等到 Decide
	if err := r.Add(Rule{Action: Direct, Kind: KindCIDR, Value: "not-a-cidr"}); err == nil {
		t.Error("bad cidr should be rejected at Add time")
	}
}
