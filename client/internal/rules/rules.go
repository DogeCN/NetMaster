// Package rules 决定一条连接走直连还是代理。
//
// 匹配顺序按 PRD §6.3 固定：私网/回环 → 域名精确 → 域名后缀 → 关键字 →
// IP CIDR → GeoIP(CN) → 端口 → 兜底。顺序写死在 Match 里，而不是让规则
// 自带优先级互相覆盖 —— 规则集是第三方每日构建的，只有"这份列表是直连类还是
// 代理类"可信，先后必须我们自己定。
//
// 与 v0.1.x 的 internal/route 相比：本包只输出 direct/proxy 两个动作，没有
// "auto"。主机不在任何列表里时由 GeoIP 阶段判断归属，判断不出来就走兜底，
// 不需要第三种状态。
package rules

import (
	"encoding/binary"
	"net/netip"
	"strconv"
	"strings"
)

// Action 出口决策。
type Action string

const (
	Direct Action = "direct"
	Proxy  Action = "proxy"
)

// Kind 规则匹配类型。
type Kind string

const (
	KindExact   Kind = "exact"    // host == value
	KindSuffix  Kind = "suffix"   // host == value || host 以 .value 结尾
	KindKeyword Kind = "keyword"  // host 含 value 子串
	KindCIDR    Kind = "cidr"     // IP 落在 value 网段
	KindGeoIPCN Kind = "geoip-cn" // 主机归属中国大陆
	KindPort    Kind = "port"     // 目标端口命中（"25" 或 "1000-2000"）
)

// Rule 一条分流规则。
type Rule struct {
	Kind   Kind
	Value  string
	Action Action
}

// GeoResolver 判断一个主机是否在中国大陆。
//
// 签名与 internal/selector.GeoResolver 一致，所以 *geoip.Provider 可以直接传入。
type GeoResolver interface {
	IsCNHost(host string) (isCN, known bool)
}

// Router 按主机与端口决定直连或代理。构造后只读，可并发使用。
type Router struct {
	fallback  Action
	geo       GeoResolver
	geoAction Action // GeoIP 阶段的动作，默认 direct（PRD §6.3：CN → direct）

	exact   map[string]Action
	suffix  map[string]Action
	keyword []kwRule
	cidr    []cidrRule
	port    []portRule

	// source 说明这次规则来自哪里；skipped 是被丢弃的规则/行数，stats 是
	// 逐列表的解析统计 —— 三者都只用于日志。
	source  string
	version string
	skipped int
	stats   []ParseStats
}

type kwRule struct {
	value  string
	action Action
}

type portRule struct {
	lo, hi uint16
	action Action
}

type cidrRule struct {
	action Action
	// v4 为真时走 uint32 前缀比较（快路径），否则退回 netip.Prefix.Contains
	// 处理 IPv6。规则量在千级，线性扫描足够。
	v4     bool
	start  uint32
	mask   uint32
	prefix netip.Prefix
}

// New 用给定规则构造 Router。fallback 是没命中任何规则时的动作。
//
// 不返回 error：规则集来自第三方，"一行写坏了"不该让整个客户端起不来，所以
// 无法识别的规则（坏 CIDR、坏端口、未知 Kind）直接跳过并计数，数量由
// Skipped 暴露给日志。
func New(rules []Rule, fallback Action, geo GeoResolver) *Router {
	if fallback != Direct && fallback != Proxy {
		fallback = Proxy
	}
	r := &Router{
		fallback:  fallback,
		geo:       geo,
		geoAction: Direct,
		exact:     make(map[string]Action),
		suffix:    make(map[string]Action),
	}
	for _, rule := range rules {
		r.add(rule)
	}
	return r
}

func (r *Router) add(rule Rule) {
	if rule.Action != Direct && rule.Action != Proxy {
		r.skipped++
		return
	}
	value := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(rule.Value)), ".")
	// 空值只在 geoip-cn 上合法（它不看 Value），其余一律算坏规则。
	if value == "" && rule.Kind != KindGeoIPCN {
		r.skipped++
		return
	}
	switch rule.Kind {
	case KindExact:
		if _, dup := r.exact[value]; !dup {
			r.exact[value] = rule.Action
		}
	case KindSuffix:
		if _, dup := r.suffix[value]; !dup {
			r.suffix[value] = rule.Action
		}
	case KindKeyword:
		r.keyword = append(r.keyword, kwRule{value: value, action: rule.Action})
	case KindCIDR:
		if c, ok := parseCIDR(value); ok {
			c.action = rule.Action
			r.cidr = append(r.cidr, c)
		} else {
			r.skipped++
		}
	case KindGeoIPCN:
		// GeoIP 阶段固定执行，规则只用来改它的动作（末条生效；默认 CN→direct）。
		r.geoAction = rule.Action
	case KindPort:
		if p, ok := parsePort(value); ok {
			p.action = rule.Action
			r.port = append(r.port, p)
		} else {
			r.skipped++
		}
	default:
		r.skipped++
	}
}

// BuiltinOnly 只用内置规则集构造 Router，**不拉任何网络、不读磁盘缓存**。
//
// 为什么砍掉订阅：分流表的唯一作用是省掉"第一次走错"。而现在客户端已经有完整的观测闭环
// （明文探测 → 判阻断 → 分片 → 记忆，见 internal/proxy/tlsfrag.go），走错一次的代价是
// 多等一个探测窗口，不是走不通。订阅反而是整个启动路径上**唯一必须外网可达的配置来源**
// —— 实测 `raw.githubusercontent.com` 在大陆直接 ECONNRESET，也就是说这份表在需要的
// 地方恰恰拿不到，还得为"拉不到"再写一条降级路径。少一个外部依赖是净收益。
//
// 砍掉它**不会伤到两条边界**，它们都独立于规则集：
//   - 私网/loopback 强制直连：`Match` 里 `privateCIDRs` 是硬编码的（见 Match）；
//   - CN 先验：GeoIP 是独立阶段（`geoAction`，默认 Direct），不经过规则列表。
//
// 所以最终形态 = 私网硬规则 + geoip CN 先验 + 进程内记忆（frag 6h / blocked 冷却）。
//
// userRules 非空时（`--rules` 指定的本地文件）叠加在内置集之上：用户显式指定了规则，
// 就该只有他的规则 + 内置兜底，不再叠订阅。
func BuiltinOnly(userRules []Rule, geo GeoResolver) (*Router, error) {
	var parsed []Rule
	skipped := 0
	var stats []ParseStats
	for _, s := range BuiltinRulesets() {
		rules, st := ParseClashRuleset(s.Name, s.Body, s.Action)
		parsed = append(parsed, rules...)
		skipped += st.Skipped
		stats = append(stats, st)
	}
	parsed = append(parsed, userRules...)
	r := New(parsed, Proxy, geo)
	r.source = "builtin"
	r.skipped = skipped
	r.stats = stats
	return r, nil
}

// Source 返回规则来源："fetched" / "cache" / "builtin"（供启动日志）。
func (r *Router) Source() string { return r.source }

// Version 返回这份规则集的版本（规则集每日构建，日期即版本）。
func (r *Router) Version() string { return r.version }

// Skipped 返回被丢弃的规则条数（坏 CIDR、坏端口、未知 Kind）之和。
func (r *Router) Skipped() int { return r.skipped }

// Stats 返回逐列表的解析统计（含第三方列表里认不出来的行数）。
func (r *Router) Stats() []ParseStats {
	out := make([]ParseStats, len(r.stats))
	copy(out, r.stats)
	return out
}

// SkippedLines 返回第三方列表里被跳过的行数总和。
//
// 与 Skipped 分开统计：前者是"列表里有我们不支持的东西"（正常，记个数就行），
// 后者是"我们自己索引不了"（说明构造参数有问题）。
func (r *Router) SkippedLines() int {
	n := 0
	for _, s := range r.stats {
		n += s.Skipped
	}
	return n
}

// Size 返回已索引的规则条数（精确+后缀+关键字+CIDR+端口）。
func (r *Router) Size() int {
	return len(r.exact) + len(r.suffix) + len(r.keyword) + len(r.cidr) + len(r.port)
}

// Match 对目标主机做分流决策。
//
// host 可以是域名或 IP 字面量，允许带端口（"host:443"、"[::1]:443"）；
// port 为 0 时表示调用方不知道端口（例如 SOCKS5 未解析出端口），端口规则
// 此时不参与。
//
// 域名不做预解析（PRD：域名原样传给服务端），所以 IP CIDR 只对"输入本来就是
// IP 字面量"的请求生效；域名的归属判断交给 GeoIP 阶段，那一步在 geoip 包
// 内部解析，不走我们的热路径。
func (r *Router) Match(host string, port uint16) Action {
	h, p := splitHost(host, port)
	if h == "" {
		return r.fallback
	}
	if isIPLiteral(h) {
		if addr, err := netip.ParseAddr(h); err == nil {
			for i := range privateCIDRs {
				if privateCIDRs[i].match(addr) {
					return Direct
				}
			}
			for i := range r.cidr {
				if r.cidr[i].match(addr) {
					return r.cidr[i].action
				}
			}
			if r.geo != nil {
				if isCN, known := r.geo.IsCNHost(h); known && isCN {
					return r.geoAction
				}
			}
			if a, ok := r.portAction(p); ok {
				return a
			}
			return r.fallback
		}
	}
	if h == "localhost" {
		// 回环域名按 127.0.0.1 处理：它和私网一样，服务端永远连不到。
		return Direct
	}
	if a, ok := r.exact[h]; ok {
		return a
	}
	if a, ok := r.suffixAction(h); ok {
		return a
	}
	if a, ok := r.keywordAction(h); ok {
		return a
	}
	if r.geo != nil {
		if isCN, known := r.geo.IsCNHost(h); known && isCN {
			return r.geoAction
		}
	}
	if a, ok := r.portAction(p); ok {
		return a
	}
	return r.fallback
}

// suffixAction 从最长到最短试各个后缀：先整体（Clash 的 DOMAIN-SUFFIX 含自身），
// 再逐个点号切。notexample.com 不会命中 example.com，因为只切在 '.' 边界上。
func (r *Router) suffixAction(h string) (Action, bool) {
	if a, ok := r.suffix[h]; ok {
		return a, true
	}
	for i := 0; i < len(h); i++ {
		if h[i] == '.' {
			if a, ok := r.suffix[h[i+1:]]; ok {
				return a, true
			}
		}
	}
	return "", false
}

func (r *Router) keywordAction(h string) (Action, bool) {
	for i := range r.keyword {
		if strings.Contains(h, r.keyword[i].value) {
			return r.keyword[i].action, true
		}
	}
	return "", false
}

func (r *Router) portAction(p uint16) (Action, bool) {
	if p == 0 {
		return "", false
	}
	for i := range r.port {
		if p >= r.port[i].lo && p <= r.port[i].hi {
			return r.port[i].action, true
		}
	}
	return "", false
}

// match 判断单个网段是否命中。整条 PRD 顺序在 Match 里，这里只管一条。
//
// netip.Addr 是值类型（不像 net.IP 那样每次 ParseIP 都要分配一段内存），
// 而 Match 在每条连接的路径上被调用 —— 每条连接少一次分配。
func (c cidrRule) match(addr netip.Addr) bool {
	if addr.Is4() && c.v4 {
		a := addr.As4() // 值类型，不分配
		return binary.BigEndian.Uint32(a[:])&c.mask == c.start
	}
	return c.prefix.IsValid() && c.prefix.Contains(addr)
}

func parseCIDR(s string) (cidrRule, bool) {
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		return cidrRule{}, false
	}
	c := cidrRule{prefix: prefix}
	// 只有 v4 才走 uint32 快路径；::ffff:0:0/120 这类"看起来像 v4"的 v6 段
	// 留给 Prefix.Contains，免得掩码换算出错。
	if prefix.Addr().Is4() {
		c.v4 = true
		c.mask = maskOf(prefix.Bits())
		a := prefix.Addr().As4()
		c.start = binary.BigEndian.Uint32(a[:]) & c.mask
	}
	return c, true
}

func maskOf(ones int) uint32 {
	switch {
	case ones <= 0:
		return 0
	case ones >= 32:
		return ^uint32(0)
	default:
		return ^uint32(0) << (32 - ones)
	}
}

func parsePort(s string) (portRule, bool) {
	if i := strings.IndexByte(s, '-'); i > 0 {
		lo, err1 := strconv.ParseUint(s[:i], 10, 16)
		hi, err2 := strconv.ParseUint(s[i+1:], 10, 16)
		if err1 != nil || err2 != nil || lo > hi {
			return portRule{}, false
		}
		return portRule{lo: uint16(lo), hi: uint16(hi)}, true
	}
	v, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return portRule{}, false
	}
	return portRule{lo: uint16(v), hi: uint16(v)}, true
}

// splitHost 归一化主机名并取出端口。保留调用方给的 port，只在它为 0 时用
// host 里自带的那个。
func splitHost(host string, port uint16) (string, uint16) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", port
	}
	if host[0] == '[' {
		if i := strings.IndexByte(host, ']'); i > 0 {
			h := strings.TrimSuffix(host[1:i], ".")
			if port == 0 && i+1 < len(host) && host[i+1] == ':' {
				if v, err := strconv.ParseUint(host[i+2:], 10, 16); err == nil {
					port = uint16(v)
				}
			}
			return strings.ToLower(h), port
		}
	}
	// 只在"恰好一个冒号且冒号后全是数字"时才切端口：那是 host:port 形态。
	// 裸 IPv6（"::1"、"2001:db8::1"）一定含两个以上冒号，不会被误切 ——
	// 按"最后一个冒号"切会把它切成 "2001:db8:"，这曾经让所有 v6 网段失配。
	if strings.Count(host, ":") == 1 {
		i := strings.IndexByte(host, ':')
		tail := host[i+1:]
		if tail != "" && isAllDigits(tail) {
			if port == 0 {
				if v, err := strconv.ParseUint(tail, 10, 16); err == nil {
					port = uint16(v)
				}
			}
			host = host[:i]
		}
	}
	return strings.ToLower(strings.TrimSuffix(host, ".")), port
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// isIPLiteral 快速排除域名。netip.ParseAddr 对域名要做完整解析，而绝大多数
// 输入是域名 —— 先扫一遍字符集，含 g-z 或 '-' 的一定不是 IP 字面量。
func isIPLiteral(h string) bool {
	for i := 0; i < len(h); i++ {
		switch c := h[i]; {
		case c >= '0' && c <= '9', c == '.', c == ':':
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// privateCIDRs 私网/回环/保留网段。命中即强制直连，优先于一切规则：
// 服务端 connect() 禁连私网，送过去必然失败。
var privateCIDRs = mustParseCIDRs([]string{
	// IPv4
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
	// IPv6
	"::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
})

func mustParseCIDRs(list []string) []cidrRule {
	out := make([]cidrRule, 0, len(list))
	for _, s := range list {
		c, ok := parseCIDR(s)
		if !ok {
			// 这是编译期常量，写错了必须在启动前炸出来，而不是安静地少一个网段。
			panic("rules: bad builtin cidr " + s)
		}
		out = append(out, c)
	}
	return out
}
