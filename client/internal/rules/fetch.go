package rules

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"netmaster/internal/cache"
)

// 内置规则集源（Loyalsoldier/clash-rules，release 分支为每日构建）。
//
// 集中在这里是为了让测试能把整组 URL 换成 httptest 的地址，也让用户能通过
// config.json 覆盖（见 Overrides）。四个列表各自对应一个动作：direct/private
// 直连，proxy/gfw 代理。
const (
	DefaultDirectURL  = "https://raw.githubusercontent.com/Loyalsoldier/clash-rules/release/direct.txt"
	DefaultProxyURL   = "https://raw.githubusercontent.com/Loyalsoldier/clash-rules/release/proxy.txt"
	DefaultPrivateURL = "https://raw.githubusercontent.com/Loyalsoldier/clash-rules/release/private.txt"
	DefaultGFWURL     = "https://raw.githubusercontent.com/Loyalsoldier/clash-rules/release/gfw.txt"
)

// RulesetSource 一个待拉取的列表：名字 + 地址 + 整份列表的动作。
type RulesetSource struct {
	Name   string
	URL    string
	Action Action
}

// DefaultSources 内置源。顺序即合并顺序。
func DefaultSources() []RulesetSource {
	return []RulesetSource{
		{Name: "private", URL: DefaultPrivateURL, Action: Direct},
		{Name: "direct", URL: DefaultDirectURL, Action: Direct},
		{Name: "proxy", URL: DefaultProxyURL, Action: Proxy},
		{Name: "gfw", URL: DefaultGFWURL, Action: Proxy},
	}
}

// Overrides 允许用户覆盖内置源地址；留空用默认。
//
// 只接受 URL 字符串，不认识 config.json —— 配置怎么存是 config 包的事。
type Overrides struct {
	Direct  string
	Proxy   string
	Private string
	GFW     string
	// Custom 是调用方已解析好的自定义规则（--rules 指向本地文件）。
	// 非空时内置源整个不拉：用户显式指定了规则集，再叠一层内置源只会让
	// "为什么这个域名走了代理"变得没法回答。
	Custom []Rule
}

// Sources 在默认源上套用用户覆盖。
func (o Overrides) Sources() []RulesetSource {
	srcs := DefaultSources()
	for i := range srcs {
		var v string
		switch srcs[i].Name {
		case "direct":
			v = o.Direct
		case "proxy":
			v = o.Proxy
		case "private":
			v = o.Private
		case "gfw":
			v = o.GFW
		}
		if v != "" {
			srcs[i].URL = v
		}
	}
	return srcs
}

var (
	// FetchBudget 是全部源的总拉取预算。超时就用已到手的那几份，剩下的用缓存
	// 或兜底补 —— 启动不该被一个慢源拖住。
	FetchBudget = 3 * time.Second
	// FetchTimeout 是单个源的超时，兜住"连接已建立但一直不返回"的情况。
	FetchTimeout = 3 * time.Second
	// MaxBody 限制单份列表大小：这些源是几百 KB 的文本，超过就是异常。
	MaxBody = 8 << 20
	// httpClient 可替换，测试用来注入假传输。
	httpClient = http.DefaultClient
)

const (
	cacheNamespace = "rules"
	// VersionLayout 是缓存的版本键：规则集是每日构建，日期就是版本号。
	VersionLayout = "2006-01-02"
)

// rulesetCache 落盘的规则集快照。
type rulesetCache struct {
	Version string            `json:"version"`
	Bodies  map[string]string `json:"bodies"`
}

// LoadRules 拉取/缓存/兜底三选一地装载规则集，返回可直接用的 Router。
//
// 顺序：并行拉取（预算 FetchBudget）→ 解析合并 → 成功写缓存 → 失败读缓存 →
// 都没有就用内置兜底集。来源由 Router.Source() 给出：fetched / cache / builtin。
//
// cachePath 是这份规则集在 cache 包里的标识（留空用 "default"）；geo 用于
// GeoIP 阶段，传 nil 则该阶段不参与。
func LoadRules(ctx context.Context, files Overrides, cachePath string, geo GeoResolver) (*Router, error) {
	if len(files.Custom) > 0 {
		return New(files.Custom, Proxy, geo), nil
	}
	return loadRules(ctx, files.Sources(), cachePath, geo, Proxy)
}

// LoadRulesFrom 同 LoadRules，但直接给源列表（测试注入 httptest 地址用）。
func LoadRulesFrom(ctx context.Context, srcs []RulesetSource, cachePath string, geo GeoResolver, fallback Action) (*Router, error) {
	return loadRules(ctx, srcs, cachePath, geo, fallback)
}

func loadRules(ctx context.Context, srcs []RulesetSource, cachePath string, geo GeoResolver, fallback Action) (*Router, error) {
	if cachePath == "" {
		cachePath = "default"
	}
	version := time.Now().UTC().Format(VersionLayout)

	bodies, fetched := fetchAll(ctx, srcs)
	source := "builtin"
	stats := make([]ParseStats, 0, len(srcs))

	if fetched > 0 && len(bodies) < len(srcs) {
		// 部分源挂了：缺的那几份用缓存补，别因为一次抖动就丢掉一整类规则。
		if c, ok := loadCache(cachePath); ok {
			for _, s := range srcs {
				if _, have := bodies[s.Name]; !have {
					if b, ok2 := c.Bodies[s.Name]; ok2 && b != "" {
						bodies[s.Name] = b
					}
				}
			}
		}
	}
	if fetched > 0 {
		source = "fetched"
		_ = cache.Save(cacheNamespace, cachePath, rulesetCache{Version: version, Bodies: bodies})
	} else if c, ok := loadCache(cachePath); ok {
		source = "cache"
		bodies = c.Bodies
		version = c.Version
	}

	var all []Rule
	if len(bodies) > 0 {
		for _, s := range srcs {
			body, ok := bodies[s.Name]
			if !ok || body == "" {
				continue
			}
			rs, st := ParseClashRuleset(s.Name, body, s.Action)
			stats = append(stats, st)
			all = append(all, rs...)
		}
	}
	if len(all) == 0 {
		// 缓存也坏了：用内置兜底集。
		source = "builtin"
		for _, s := range BuiltinRulesets() {
			rs, st := ParseClashRuleset(s.Name, s.Body, s.Action)
			stats = append(stats, st)
			all = append(all, rs...)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("rules: no usable rule from %d sources", len(srcs))
	}

	// 兜底集自带私网网段，但它同时也会被解析成 cidr 规则 —— 无妨：内置
	// privateCIDRs 优先，两套一致。
	r := New(all, fallback, geo)
	r.source = source
	r.version = version
	r.stats = stats
	return r, nil
}

// fetchAll 并行拉取，返回成功拿到的正文（name → body）与成功数。
//
// 预算到期就返回已到手的部分：一个源慢不该让启动卡在那里。goroutine 仍会
// 继续跑，但结果写进有缓冲的 channel，不会泄漏。
func fetchAll(ctx context.Context, srcs []RulesetSource) (map[string]string, int) {
	fetchCtx, cancel := context.WithTimeout(ctx, FetchBudget)
	defer cancel()

	type result struct {
		name string
		body string
		err  error
	}
	ch := make(chan result, len(srcs))
	var wg sync.WaitGroup
	for _, s := range srcs {
		if s.URL == "" {
			ch <- result{name: s.Name, err: fmt.Errorf("empty url")}
			continue
		}
		wg.Add(1)
		go func(s RulesetSource) {
			defer wg.Done()
			body, err := fetchOne(fetchCtx, s.URL)
			ch <- result{name: s.Name, body: body, err: err}
		}(s)
	}
	go func() { wg.Wait() }()

	got := make(map[string]string, len(srcs))
	for remaining := len(srcs); remaining > 0; {
		select {
		case r := <-ch:
			remaining--
			if r.err == nil && r.body != "" {
				got[r.name] = r.body
			}
		case <-fetchCtx.Done():
			remaining = 0 // 预算用尽：用已到手的
		}
	}
	return got, len(got)
}

func fetchOne(ctx context.Context, url string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "netmaster")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d from %s", resp.StatusCode, url)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(MaxBody)))
	if err != nil && len(b) == 0 {
		return "", err
	}
	// 源偶发返回 HTML 错误页（代理页/404 页）：按内容再判一次，别把一页 HTML
	// 当成规则集塞进缓存（那样"成功"了却一条规则都没有）。
	if !looksLikeRuleset(string(b)) {
		return "", fmt.Errorf("unexpected content from %s", url)
	}
	return string(b), nil
}

// looksLikeRuleset 判断正文里是否至少有一行能认出来的 Clash 规则。
func looksLikeRuleset(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		head := strings.ToUpper(text)
		for _, prefix := range []string{"DOMAIN,", "DOMAIN-SUFFIX,", "DOMAIN-KEYWORD,", "IP-CIDR", "IP-CIDR6"} {
			if len(head) >= len(prefix) && head[:len(prefix)] == prefix {
				return true
			}
		}
	}
	return false
}

func loadCache(id string) (rulesetCache, bool) {
	var c rulesetCache
	if _, ok := cache.Load(cacheNamespace, id, &c); !ok || len(c.Bodies) == 0 {
		return rulesetCache{}, false
	}
	return c, true
}
