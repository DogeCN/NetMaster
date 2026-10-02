package rules

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"netmaster/internal/cache"
)

// fakeServer 起一个假的内置源服务器：
//   - newFakeServer(t, mode)  按 mode 决定行为（见下方 modes）
//   - newFakeBodyServer(t, b) 固定返回 b，用来在同一份缓存里写入可区分的内容
//
// 让测试能验证"缺的那份列表是从缓存恢复的"，而不是看着四个源返回同样的
// 内容蒙混过关。
type fakeServer struct {
	*httptest.Server
	url  string
	hits atomic.Int32
}

// modes 是 newFakeServer 支持的行为名。
//   - ok      正常返回规则行
//   - slow    挂住直到请求上下文取消（模拟超时）
//   - http500 返回 HTTP 500
//   - html    返回 HTML 错误页（200 但内容不是规则集）
func newFakeServer(t *testing.T, mode string) *fakeServer {
	return newFakeBodyServer(t, mode, "DOMAIN-SUFFIX,fake.example\n")
}

func newFakeBodyServer(t *testing.T, mode, body string) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		switch mode {
		case "slow":
			<-r.Context().Done() // 卡到调用方放弃
		case "http500":
			w.WriteHeader(http.StatusInternalServerError)
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>404 not found</body></html>"))
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(fs.Close)
	fs.url = fs.Server.URL
	return fs
}

// stubClient 把 httpClient 换成指定实现；返回还原函数。
func stubClient(c *http.Client) func() {
	old := httpClient
	httpClient = c
	return func() { httpClient = old }
}

// isolateCache 把缓存目录指到临时目录：cache 包用的是真实的用户缓存目录，
// 测试之间会互相污染，也会在跑测试的机器上留下文件。
func isolateCache(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LocalAppData", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
}

// allSources 把四个源都指向同一个地址（测试里不关心列表差异）。
func allSources(url string) []RulesetSource {
	return []RulesetSource{
		{Name: "private", URL: url, Action: Direct},
		{Name: "direct", URL: url, Action: Direct},
		{Name: "proxy", URL: url, Action: Proxy},
		{Name: "gfw", URL: url, Action: Proxy},
	}
}

// 成功：四个源都返回，来源应是 fetched，规则可用。
func TestLoadRulesFetched(t *testing.T) {
	isolateCache(t)
	srv := newFakeServer(t, "ok")
	// 用服务端自己的 Client，避免 http.DefaultClient 的连接池影响计时
	defer stubClient(srv.Client())()

	r, err := LoadRulesFrom(context.Background(), allSources(srv.url), "test-fetched", nil, Proxy)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := r.Source(); got != "fetched" {
		t.Errorf("Source() = %q, want fetched", got)
	}
	if r.Size() == 0 {
		t.Error("no rules indexed")
	}
	if got := r.Match("x.fake.example", 443); got != Direct {
		t.Errorf("direct list action: got %q, want direct", got)
	}
	if r.Version() == "" {
		t.Error("version should be set")
	}
}

// 成功之后要写缓存，下一次就能离线用。
func TestLoadRulesWritesCache(t *testing.T) {
	isolateCache(t)
	srv := newFakeServer(t, "ok")
	defer stubClient(srv.Client())()

	if _, err := LoadRulesFrom(context.Background(), allSources(srv.url), "test-cached", nil, Proxy); err != nil {
		t.Fatalf("first load: %v", err)
	}
	var c rulesetCache
	age, ok := cache.Load(cacheNamespace, "test-cached", &c)
	if !ok {
		t.Fatal("cache was not written")
	}
	if age < 0 || len(c.Bodies) != 4 {
		t.Errorf("cached bodies = %d, want 4", len(c.Bodies))
	}
	if c.Version == "" {
		t.Error("cached version empty")
	}
}

// 全失败 + 有缓存 → 来源 cache。
func TestLoadRulesFallsBackToCache(t *testing.T) {
	isolateCache(t)
	srv := newFakeServer(t, "ok")
	defer stubClient(srv.Client())()

	if _, err := LoadRulesFrom(context.Background(), allSources(srv.url), "test-fallback", nil, Proxy); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	// 换成必然失败的 Client：所有请求都被拒绝
	dead := &http.Client{Transport: errRoundTripper{}}
	defer stubClient(dead)()

	r, err := LoadRulesFrom(context.Background(), allSources(srv.url), "test-fallback", nil, Proxy)
	if err != nil {
		t.Fatalf("load with dead sources: %v", err)
	}
	if got := r.Source(); got != "cache" {
		t.Errorf("Source() = %q, want cache", got)
	}
	if got := r.Match("x.fake.example", 443); got != Direct {
		t.Errorf("cached rules should work: got %q", got)
	}
}

// 全失败 + 无缓存 → 来源 builtin，且私网/常见域名仍可用。
func TestLoadRulesFallsBackToBuiltin(t *testing.T) {
	isolateCache(t)
	defer stubClient(&http.Client{Transport: errRoundTripper{}})()

	r, err := LoadRulesFrom(context.Background(), allSources("http://127.0.0.1:1/nope"), "test-builtin", nil, Proxy)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := r.Source(); got != "builtin" {
		t.Errorf("Source() = %q, want builtin", got)
	}
	if got := r.Match("127.0.0.1", 443); got != Direct {
		t.Errorf("builtin private: got %q, want direct", got)
	}
	if got := r.Match("www.google.com", 443); got != Proxy {
		t.Errorf("builtin gfw: got %q, want proxy", got)
	}
	if len(r.Stats()) == 0 {
		t.Error("Stats() should describe the builtin parse")
	}
}

// 总预算内用一个慢源：超时后应当用已到手的那几份，而不是干等到死。
func TestLoadRulesBudgetTimeout(t *testing.T) {
	isolateCache(t)
	old := FetchBudget
	FetchBudget = 300 * time.Millisecond
	defer func() { FetchBudget = old }()

	fast := newFakeServer(t, "ok")
	slow := newFakeServer(t, "slow")
	defer stubClient(&http.Client{})()

	srcs := []RulesetSource{
		{Name: "private", URL: fast.url, Action: Direct},
		{Name: "gfw", URL: slow.url, Action: Proxy},
	}
	start := time.Now()
	r, err := LoadRulesFrom(context.Background(), srcs, "test-budget", nil, Proxy)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if elapsed > old {
		t.Errorf("took %s, should have given up within the original budget %s", elapsed, old)
	}
	if got := r.Source(); got != "fetched" {
		t.Errorf("Source() = %q, want fetched (partial success is still fetched)", got)
	}
	if got := r.Match("x.fake.example", 443); got != Direct {
		t.Errorf("the fast source should have contributed: got %q", got)
	}
	if elapsed < FetchBudget {
		t.Errorf("returned too early (%s), should have waited out the budget", elapsed)
	}
}

// HTTP 500 与 HTML 错误页都要算失败，不能被当成"成功但没规则"写进缓存。
func TestLoadRulesRejectsBadContent(t *testing.T) {
	isolateCache(t)
	defer stubClient(&http.Client{})()

	for _, mode := range []string{"http500", "html"} {
		srv := newFakeServer(t, mode)
		r, err := LoadRulesFrom(context.Background(), allSources(srv.url), "test-bad-"+mode, nil, Proxy)
		if err != nil {
			t.Fatalf("%s: load: %v", mode, err)
		}
		if got := r.Source(); got != "builtin" {
			t.Errorf("%s: Source() = %q, want builtin", mode, got)
		}
		srv.Close()
	}
}

// 部分源失败：缺的那份从缓存补，不要因为一次抖动丢掉一整类规则。
func TestLoadRulesPartialFailureUsesCache(t *testing.T) {
	isolateCache(t)
	defer stubClient(&http.Client{})()

	// 第一轮：两个源各返回一条可区分的规则，写进缓存。
	directSrv := newFakeBodyServer(t, "ok", "DOMAIN-SUFFIX,from-net.example\n")
	gfwSrv := newFakeBodyServer(t, "ok", "DOMAIN-SUFFIX,from-cache.example\n")
	srcs := []RulesetSource{
		{Name: "direct", URL: directSrv.url, Action: Direct},
		{Name: "gfw", URL: gfwSrv.url, Action: Proxy},
	}
	if _, err := LoadRulesFrom(context.Background(), srcs, "test-partial", nil, Proxy); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// 第二轮：direct 正常，gfw 挂了 → gfw 那一份应当从缓存回来
	// （proxy 动作 → from-cache.example 应当是 proxy）。
	deadSrv := newFakeServer(t, "http500")
	srcs2 := []RulesetSource{
		{Name: "direct", URL: directSrv.url, Action: Direct},
		{Name: "gfw", URL: deadSrv.url, Action: Proxy},
	}
	r, err := LoadRulesFrom(context.Background(), srcs2, "test-partial", nil, Proxy)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got := r.Source(); got != "fetched" {
		t.Errorf("Source() = %q, want fetched", got)
	}
	if got := r.Match("x.from-net.example", 443); got != Direct {
		t.Errorf("direct list still works: got %q", got)
	}
	if got := r.Match("x.from-cache.example", 443); got != Proxy {
		t.Errorf("gfw list should come from cache: got %q, want proxy", got)
	}
}

// 上级 ctx 已经取消时也要能返回（不能卡住）。
func TestLoadRulesRespectsContext(t *testing.T) {
	isolateCache(t)
	defer stubClient(&http.Client{})()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := LoadRulesFrom(ctx, allSources("http://127.0.0.1:1/x"), "test-cancel", nil, Proxy)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := r.Source(); got != "builtin" {
		t.Errorf("Source() = %q, want builtin (cancelled ctx → no fetch)", got)
	}
}

// Overrides 让用户用 config.json 覆盖源地址。
func TestOverrides(t *testing.T) {
	base := Overrides{}
	srcs := base.Sources()
	if len(srcs) != 4 {
		t.Fatalf("default sources = %d, want 4", len(srcs))
	}
	for i, want := range []string{DefaultPrivateURL, DefaultDirectURL, DefaultProxyURL, DefaultGFWURL} {
		if srcs[i].URL != want {
			t.Errorf("source %d url = %q, want %q", i, srcs[i].URL, want)
		}
	}
	o := Overrides{Direct: "http://d.local/d.txt", Proxy: "http://p.local/p.txt"}
	s2 := o.Sources()
	if s2[1].URL != "http://d.local/d.txt" {
		t.Errorf("direct override not applied: %q", s2[1].URL)
	}
	if s2[2].URL != "http://p.local/p.txt" {
		t.Errorf("proxy override not applied: %q", s2[2].URL)
	}
	// 没覆盖的保持默认
	if s2[3].URL != DefaultGFWURL {
		t.Errorf("gfw should keep the default: %q", s2[3].URL)
	}
}

// LoadRules 走 wire Method Overrides 的路径也要能落到同一个实现上。
func TestLoadRulesEntryPoint(t *testing.T) {
	isolateCache(t)
	srv := newFakeServer(t, "ok")
	defer stubClient(srv.Client())()

	r, err := LoadRules(context.Background(), Overrides{
		Direct:  srv.url,
		Proxy:   srv.url,
		Private: srv.url,
		GFW:     srv.url,
	}, "test-entry", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := r.Source(); got != "fetched" {
		t.Errorf("Source() = %q, want fetched", got)
	}
}

// errRoundTripper 让所有请求立刻失败（模拟网络不可达）。
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network unreachable")
}
