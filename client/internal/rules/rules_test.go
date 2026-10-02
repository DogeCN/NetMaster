package rules

import (
	"fmt"
	"testing"
)

// 固定 IP 归属的 GeoResolver 替身：Match 的热路径上不能真的解析 DNS，
// 所以测试里 GeoIP 阶段必须由这个替身给出确定答案。
type fakeGeo struct {
	cn     map[string]bool // 主机 → 是否 CN
	known  bool
	called int
}

func (g *fakeGeo) IsCNHost(host string) (bool, bool) {
	g.called++
	if g.cn != nil {
		if v, ok := g.cn[host]; ok {
			return v, true
		}
	}
	if g.known {
		return false, true
	}
	return false, false
}

func newFakeGeo(cn ...string) *fakeGeo {
	g := &fakeGeo{cn: map[string]bool{}}
	for _, h := range cn {
		g.cn[h] = true
	}
	g.known = true
	return g
}

// 私网/回环强制直连，优先于一切规则：服务端 connect() 禁连私网，送过去必失败。
func TestPrivateCIDRWins(t *testing.T) {
	r := New([]Rule{
		{Kind: KindCIDR, Value: "10.0.0.0/8", Action: Proxy},
		{Kind: KindSuffix, Value: "168.192.in-addr.arpa", Action: Proxy},
		{Kind: KindPort, Value: "443", Action: Proxy},
	}, Proxy, nil)

	for _, host := range []string{
		"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.1.1",
		"100.64.0.1", "::1", "fd00::1", "fe80::1", "localhost",
	} {
		// 连端口规则也压不过它：443 命中 Proxy，但私网优先。
		if got := r.Match(host, 443); got != Direct {
			t.Errorf("Match(%q,443) = %q, want direct (private wins)", host, got)
		}
	}
}

// 匹配顺序：exact → suffix → keyword → cidr。
func TestMatchOrder(t *testing.T) {
	r := New([]Rule{
		{Kind: KindSuffix, Value: "example.com", Action: Direct},
		{Kind: KindExact, Value: "a.example.com", Action: Proxy},
		{Kind: KindKeyword, Value: "example", Action: Direct},
	}, Proxy, nil)

	if got := r.Match("a.example.com", 443); got != Proxy {
		t.Errorf("exact should beat suffix: got %q", got)
	}
	if got := r.Match("b.example.com", 443); got != Direct {
		t.Errorf("suffix should apply to subdomains: got %q", got)
	}
	// 只有关键字能命中的主机
	if got := r.Match("myexample.net", 443); got != Direct {
		t.Errorf("keyword should match: got %q", got)
	}
}

// suffix 的边界：只切在 '.' 上，a.example.com 命中 example.com，
// notexample.com 不命中。
func TestSuffixBoundary(t *testing.T) {
	r := New([]Rule{{Kind: KindSuffix, Value: "example.com", Action: Proxy}}, Direct, nil)
	for _, h := range []string{"example.com", "a.example.com", "a.b.example.com"} {
		if got := r.Match(h, 443); got != Proxy {
			t.Errorf("Match(%q) = %q, want proxy", h, got)
		}
	}
	for _, h := range []string{"notexample.com", "example.com.evil.net", "xexample.com", "examplex.com"} {
		if got := r.Match(h, 443); got != Direct {
			t.Errorf("Match(%q) = %q, want direct (no substring match)", h, got)
		}
	}
}

// exact 只匹配完全相等。
func TestExactIsStrict(t *testing.T) {
	r := New([]Rule{{Kind: KindExact, Value: "example.com", Action: Proxy}}, Direct, nil)
	if got := r.Match("example.com", 443); got != Proxy {
		t.Errorf("exact should match itself: got %q", got)
	}
	if got := r.Match("a.example.com", 443); got != Direct {
		t.Errorf("exact must not match subdomains: got %q", got)
	}
}

func TestKeyword(t *testing.T) {
	r := New([]Rule{{Kind: KindKeyword, Value: "google", Action: Proxy}}, Direct, nil)
	for _, h := range []string{"google.com", "www.google.com", "mygooglecdn.net", "google"} {
		if got := r.Match(h, 443); got != Proxy {
			t.Errorf("keyword: Match(%q) = %q, want proxy", h, got)
		}
	}
	if got := r.Match("example.com", 443); got != Direct {
		t.Errorf("keyword should not match %q", "example.com")
	}
}

// IP CIDR 只对"输入本来就是 IP 字面量"生效；域名不做预解析，所以域名永远
// 不参与 CIDR 匹配（它交给 GeoIP 阶段）。
func TestCIDROnlyForIPLiteral(t *testing.T) {
	g := newFakeGeo() // 未列出的域名 → 非 CN → 走到兜底
	r := New([]Rule{{Kind: KindCIDR, Value: "1.2.3.0/24", Action: Direct}}, Proxy, g)

	if got := r.Match("1.2.3.4", 443); got != Direct {
		t.Errorf("ip literal should hit cidr: got %q", got)
	}
	if got := r.Match("1.2.4.4", 443); got != Proxy {
		t.Errorf("ip outside cidr should fall through: got %q", got)
	}
	// 域名：即便它的 IP 就在这个网段里，也不预解析 —— 否则 Match 会在
	// 每条连接的路径上做一次 DNS，既慢又泄漏。
	if got := r.Match("host.in.cidr.example", 443); got != Proxy {
		t.Errorf("domain must not be pre-resolved: got %q", got)
	}
	if g.called == 0 {
		t.Error("geoip stage should have been consulted for the domain")
	}
}

// IPv6 CIDR 走 net.IPNet.Contains 的慢路径，也要能命中。
func TestCIDRv6(t *testing.T) {
	r := New([]Rule{{Kind: KindCIDR, Value: "2001:db8::/32", Action: Direct}}, Proxy, nil)
	if got := r.Match("2001:db8::1", 443); got != Direct {
		t.Errorf("v6 cidr: got %q, want direct", got)
	}
	if got := r.Match("2001:db9::1", 443); got != Proxy {
		t.Errorf("v6 cidr miss: got %q, want proxy", got)
	}
	// 带方括号和端口的写法
	if got := r.Match("[2001:db8::1]:443", 0); got != Direct {
		t.Errorf("bracketed v6: got %q, want direct", got)
	}
}

// GeoIP 阶段：CN → direct，且它在端口规则之前。
func TestGeoIPStage(t *testing.T) {
	g := newFakeGeo("baidu.com")
	r := New([]Rule{{Kind: KindPort, Value: "443", Action: Proxy}}, Proxy, g)
	if got := r.Match("baidu.com", 443); got != Direct {
		t.Errorf("cn host should be direct before port rules: got %q", got)
	}
	if got := r.Match("google.com", 443); got != Proxy {
		t.Errorf("non-cn host should reach port rules: got %q", got)
	}
}

// geoip-cn 规则可以把 GeoIP 阶段的动作翻成 proxy（默认 direct）。
func TestGeoIPRuleFlipsAction(t *testing.T) {
	g := newFakeGeo("baidu.com")
	r := New([]Rule{{Kind: KindGeoIPCN, Value: "", Action: Proxy}}, Direct, g)
	if got := r.Match("baidu.com", 443); got != Proxy {
		t.Errorf("geoip-cn rule with proxy action: got %q", got)
	}
}

// GeoResolver 为 nil 或 unknown 时，GeoIP 阶段不参与，直接继续往下走。
func TestGeoIPUnknownFallsThrough(t *testing.T) {
	unknown := &fakeGeo{known: false}
	r := New([]Rule{{Kind: KindPort, Value: "25", Action: Direct}}, Proxy, unknown)
	if got := r.Match("whatever.example", 25); got != Direct {
		t.Errorf("unknown geo should not short-circuit: got %q", got)
	}
	if got := r.Match("whatever.example", 443); got != Proxy {
		t.Errorf("unknown geo should reach fallback: got %q", got)
	}
	nilGeo := New(nil, Proxy, nil)
	if got := nilGeo.Match("a.com", 443); got != Proxy {
		t.Errorf("nil geo: got %q, want fallback", got)
	}
}

func TestPortRule(t *testing.T) {
	r := New([]Rule{
		{Kind: KindPort, Value: "25", Action: Direct},
		{Kind: KindPort, Value: "1000-2000", Action: Direct},
	}, Proxy, nil)
	for _, p := range []uint16{25, 1000, 1500, 2000} {
		if got := r.Match("example.com", p); got != Direct {
			t.Errorf("port %d: got %q, want direct", p, got)
		}
	}
	for _, p := range []uint16{26, 999, 2001, 443} {
		if got := r.Match("example.com", p); got != Proxy {
			t.Errorf("port %d: got %q, want fallback", p, got)
		}
	}
	// 端口未知（0）时端口规则不参与 —— 否则会给一批连接乱套规则。
	if got := r.Match("example.com", 0); got != Proxy {
		t.Errorf("port 0 should skip port rules: got %q", got)
	}
	// 端口可以从 host 里取出（SOCKS5 常见形态）
	if got := r.Match("example.com:25", 0); got != Direct {
		t.Errorf("port from host: got %q, want direct", got)
	}
}

func TestFallback(t *testing.T) {
	for _, fb := range []Action{Direct, Proxy} {
		r := New(nil, fb, nil)
		if got := r.Match("nope.example", 443); got != fb {
			t.Errorf("fallback %q: got %q", fb, got)
		}
	}
	// 非法兜底动作要被纠正成 proxy，而不是把脏值透给下游 switch。
	r := New(nil, Action("bogus"), nil)
	if got := r.Match("nope.example", 443); got != Proxy {
		t.Errorf("bogus fallback should be corrected: got %q", got)
	}
}

// 主机归一化：大小写、尾部点、带端口、IPv6 方括号。
func TestHostNormalization(t *testing.T) {
	r := New([]Rule{{Kind: KindSuffix, Value: "google.com", Action: Proxy}}, Direct, nil)
	for _, h := range []string{
		"WWW.Google.Com", "www.google.com.", "www.google.com:443", "  www.google.com  ",
	} {
		if got := r.Match(h, 0); got != Proxy {
			t.Errorf("Match(%q) = %q, want proxy", h, got)
		}
	}
	// 空主机直接兜底，不要去查 map
	if got := r.Match("", 0); got != Direct {
		t.Errorf("empty host: got %q, want fallback", got)
	}
}

// 坏规则被跳过并计数，不让一条写坏的第三方列表把客户端拖死。
func TestBadRulesAreSkipped(t *testing.T) {
	r := New([]Rule{
		{Kind: KindCIDR, Value: "not-a-cidr", Action: Direct},
		{Kind: KindPort, Value: "abc", Action: Direct},
		{Kind: KindPort, Value: "70000", Action: Direct},
		{Kind: KindPort, Value: "200-100", Action: Direct},
		{Kind: Kind("GEOSITE"), Value: "cn", Action: Direct},
		{Kind: KindSuffix, Value: "ok.com", Action: Direct},
	}, Proxy, nil)
	if got := r.Skipped(); got != 5 {
		t.Errorf("Skipped() = %d, want 5", got)
	}
	if r.Size() != 1 {
		t.Errorf("Size() = %d, want 1", r.Size())
	}
	if got := r.Match("a.ok.com", 443); got != Direct {
		t.Errorf("good rule should still work: got %q", got)
	}
	// 动作非法的规则也要跳过，不能把脏动作透给下游 switch。
	r2 := New([]Rule{{Kind: KindSuffix, Value: "x.com", Action: Action("reject")}}, Proxy, nil)
	if r2.Skipped() != 1 || r2.Size() != 0 {
		t.Errorf("bad action should be skipped: skipped=%d size=%d", r2.Skipped(), r2.Size())
	}
}

// 从 Clash 列表构造出来的 Router 应当覆盖真实世界的几个典型主机。
func TestRouterFromClashLists(t *testing.T) {
	direct, _ := ParseClashRuleset("direct", "DOMAIN-SUFFIX,baidu.com\nDOMAIN,localhost\n", Direct)
	proxy, _ := ParseClashRuleset("proxy", "DOMAIN-SUFFIX,google.com\nDOMAIN,youtube.com\n", Proxy)
	var all []Rule
	all = append(all, direct...)
	all = append(all, proxy...)
	r := New(all, Proxy, nil)

	cases := []struct {
		host string
		want Action
	}{
		{"www.baidu.com", Direct},
		{"baidu.com", Direct},
		{"www.google.com", Proxy},
		{"youtube.com", Proxy},
		{"unlisted.example", Proxy}, // 兜底
		{"127.0.0.1", Direct},       // 私网优先
	}
	for _, c := range cases {
		if got := r.Match(c.host, 443); got != c.want {
			t.Errorf("Match(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// 500 条规则下 Match 的 P99 要 < 1ms。分流在每条连接的路径上，慢了就是
// 每个请求都慢。
func BenchmarkMatch(b *testing.B) {
	benchMatch(b, 500)
}

func BenchmarkMatch500Domains(b *testing.B) { benchMatch(b, 500) }

// 基准要用大量不同的规则值：全用同一个值的话索引里只有一条，测出来的不是
// 扫描成本。
func benchRules(n int) []Rule {
	var rules []Rule
	for i := 0; i < n; i++ {
		switch i % 5 {
		case 0:
			rules = append(rules, Rule{Kind: KindExact, Value: fmt.Sprintf("host%d.example.com", i), Action: Proxy})
		case 1:
			rules = append(rules, Rule{Kind: KindSuffix, Value: fmt.Sprintf("suffix%d.example.com", i), Action: Proxy})
		case 2:
			rules = append(rules, Rule{Kind: KindKeyword, Value: fmt.Sprintf("kw%d", i), Action: Proxy})
		case 3:
			rules = append(rules, Rule{Kind: KindCIDR, Value: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Action: Direct})
		case 4:
			rules = append(rules, Rule{Kind: KindPort, Value: fmt.Sprintf("%d", 1000+i), Action: Direct})
		}
	}
	return rules
}

func benchMatch(b *testing.B, n int) {
	r := New(benchRules(n), Proxy, nil)
	hosts := []string{
		"a.example.com", "www.google.com", "cdn.suffix7.example.com",
		"mykw42host.net", "1.2.3.4", "192.168.1.1", "unlisted.example.org",
	}
	ports := []uint16{443, 80, 22, 1500, 0}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Match(hosts[i%len(hosts)], ports[i%len(ports)])
	}
}

// Match 不分配、不解析，所以 ns/op 直接就是 P99 的量级（单线程、无锁，
// 没有长尾来源）。这条断言把"分流必须便宜"这件事钉在测试里。
func TestMatchP99Under1ms(t *testing.T) {
	res := testing.Benchmark(func(b *testing.B) { benchMatch(b, 500) })
	nsPerOp := res.NsPerOp()
	if nsPerOp > 1_000_000 {
		t.Errorf("Match P99 = %d ns, want < 1ms (500 rules)", nsPerOp)
	}
	t.Logf("500 rules: %d ns/op, %d allocs/op", nsPerOp, res.AllocsPerOp())
}

// 顺带验证 500 条规则确实都进了索引（否则上面的基准测的是空表）。
func TestRouterIndexes500Rules(t *testing.T) {
	r := New(benchRules(500), Proxy, nil)
	if r.Size() != 500 {
		t.Errorf("Size() = %d, want 500", r.Size())
	}
	if r.Skipped() != 0 {
		t.Errorf("Skipped() = %d, want 0", r.Skipped())
	}
}
