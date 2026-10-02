package rules

import (
	"strings"
	"testing"
)

func TestParseClashRuleset(t *testing.T) {
	body := `# comment line
DOMAIN,example.com
DOMAIN-SUFFIX,google.com
DOMAIN-KEYWORD,google
IP-CIDR,1.1.1.0/24
IP-CIDR,8.8.8.8/32,no-resolve
IP-CIDR6,2001:db8::/32

// slash comment
GEOSITE,cn
PROCESS-NAME,curl
MATCH
DOMAIN
DOMAIN-SUFFIX,
`
	got, st := ParseClashRuleset("t", body, Proxy)
	if len(got) != 6 {
		t.Fatalf("parsed %d rules, want 6: %+v", len(got), got)
	}
	want := []Rule{
		{Kind: KindExact, Value: "example.com", Action: Proxy},
		{Kind: KindSuffix, Value: "google.com", Action: Proxy},
		{Kind: KindKeyword, Value: "google", Action: Proxy},
		{Kind: KindCIDR, Value: "1.1.1.0/24", Action: Proxy},
		{Kind: KindCIDR, Value: "8.8.8.8/32", Action: Proxy},
		{Kind: KindCIDR, Value: "2001:db8::/32", Action: Proxy},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if st.Skipped != 5 {
		t.Errorf("Skipped = %d, want 5 (GEOSITE/PROCESS-NAME/MATCH/无值/空值)", st.Skipped)
	}
	if st.Name != "t" {
		t.Errorf("stats name = %q", st.Name)
	}
	// Total 只数非空非注释行
	if st.Total != 11 {
		t.Errorf("Total = %d, want 11", st.Total)
	}
}

// IP-CIDR6 要被当成 CIDR 收下（IPv6 段由 net.IPNet.Contains 处理）。
func TestParseCIDR6(t *testing.T) {
	got, _ := ParseClashRuleset("t", "IP-CIDR6,2001:db8::/32\nIP-CIDR6,2606:4700::/32,no-resolve\n", Direct)
	if len(got) != 2 {
		t.Fatalf("got %d rules, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Kind != KindCIDR {
			t.Errorf("kind = %q, want cidr", r.Kind)
		}
		if r.Action != Direct {
			t.Errorf("action = %q, want direct", r.Action)
		}
	}
	// 解析出来的 IPv6 段要真的能匹配
	rt := New(got, Proxy, nil)
	if a := rt.Match("2001:db8::1", 443); a != Direct {
		t.Errorf("v6 match = %q, want direct", a)
	}
}

// 各种肮脏输入：CRLF、多余空格、大小写混写、尾部点。
func TestParseToleratesMessyInput(t *testing.T) {
	body := "DOMAIN-SUFFIX,Example.COM\r\n  DOMAIN , spaced.com \r\n\tDOMAIN-KEYWORD,\tkw\t\nDOMAIN-SUFFIX,trailing.dot.\n"
	got, st := ParseClashRuleset("t", body, Direct)
	if len(got) != 4 {
		t.Fatalf("got %d rules, want 4: %+v (skipped=%d)", len(got), got, st.Skipped)
	}
	// 域名统一小写、去掉尾部点
	for i, want := range []string{"example.com", "spaced.com", "kw", "trailing.dot"} {
		if got[i].Value != want {
			t.Errorf("value %d = %q, want %q", i, got[i].Value, want)
		}
	}
}

// 同一份列表里的重复条目要合并，别让索引白长一倍。
func TestParseDeduplicates(t *testing.T) {
	got, _ := ParseClashRuleset("t", "DOMAIN-SUFFIX,a.com\nDOMAIN-SUFFIX,a.com\nDOMAIN,a.com\n", Proxy)
	if len(got) != 2 {
		t.Fatalf("got %d rules, want 2 (one dedup + different kind): %+v", len(got), got)
	}
}

// 整份列表一个动作：动作由调用方给，不由列表内容决定。
func TestParseActionIsPerList(t *testing.T) {
	d, _ := ParseClashRuleset("direct", "DOMAIN-SUFFIX,a.com\n", Direct)
	p, _ := ParseClashRuleset("proxy", "DOMAIN-SUFFIX,b.com\n", Proxy)
	if d[0].Action != Direct || p[0].Action != Proxy {
		t.Fatalf("actions = %q / %q", d[0].Action, p[0].Action)
	}
	// 非法动作纠正成 proxy，不要把脏值透给下游 switch
	f, _ := ParseClashRuleset("x", "DOMAIN-SUFFIX,c.com\n", Action("reject"))
	if f[0].Action != Proxy {
		t.Errorf("bad action should be corrected, got %q", f[0].Action)
	}
}

// 空正文、纯注释正文都不能产生规则。
func TestParseEmpty(t *testing.T) {
	for _, body := range []string{"", "# only a comment\n\n", "\n\n\n"} {
		got, st := ParseClashRuleset("t", body, Proxy)
		if len(got) != 0 || st.Skipped != 0 {
			t.Errorf("body %q: got %d rules, skipped %d", body, len(got), st.Skipped)
		}
	}
}

// 内置兜底集必须能解析出规则，且私网那份覆盖主要私网网段。
func TestBuiltinRulesets(t *testing.T) {
	sets := BuiltinRulesets()
	if len(sets) != 2 {
		t.Fatalf("builtin sets = %d, want 2", len(sets))
	}
	names := map[string]Action{}
	for _, s := range sets {
		names[s.Name] = s.Action
		rs, st := ParseClashRuleset(s.Name, s.Body, s.Action)
		if len(rs) == 0 {
			t.Errorf("builtin %q produced no rules", s.Name)
		}
		if st.Skipped != 0 {
			t.Errorf("builtin %q has %d unparsable lines", s.Name, st.Skipped)
		}
	}
	if names["private"] != Direct || names["gfw"] != Proxy {
		t.Errorf("builtin actions = %v", names)
	}
	// 兜底集构造出的 Router：私网直连、gfw 域名代理
	var all []Rule
	for _, s := range sets {
		rs, _ := ParseClashRuleset(s.Name, s.Body, s.Action)
		all = append(all, rs...)
	}
	r := New(all, Proxy, nil)
	if got := r.Match("127.0.0.1", 443); got != Direct {
		t.Errorf("builtin: 127.0.0.1 = %q, want direct", got)
	}
	if got := r.Match("www.google.com", 443); got != Proxy {
		t.Errorf("builtin: google = %q, want proxy", got)
	}
	if !strings.Contains(sets[1].Body, "DOMAIN-SUFFIX,") {
		t.Error("gfw builtin should carry domain suffixes")
	}
}
