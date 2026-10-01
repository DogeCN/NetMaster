package route

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// Action 出口决策。
type Action string

const (
	Direct Action = "direct"
	Proxy  Action = "proxy"
	// Auto 交给出口层判断：按 IP 归属（CN 网段则直连）与域名亲和自适应选择，
	// 失败还会互相回退。用于"没有明确依据说它必须走哪边"的主机 —— 兜底若按
	// 后缀穷举送代理，国内 IP、没被墙的站点会被绕到海外再绕回来。
	//
	// Auto 只能作为 default，不能写成某条规则的动作：它的意思正是"没有规则命中时怎么办"，
	// 写成一条规则等于给某个域名单独声明"按默认处理"，没有意义。
	Auto Action = "auto"
)

// Kind 规则匹配类型。
//
// 只有三种，来自默认规则表的实际用法。判断标准：每留一种匹配类型都要有人真的
// 用它 —— 没被真实规则验证过的"可能性"不值得维护成本（regexp 还要求每条规则
// 持有一个编译后的正则对象）。要加回来的时候再加。
//
// full 的语义已被 domain 覆盖（domain 是 host == value || host 以 .value 结尾），
// 所以精确匹配仍然能表达。
type Kind string

const (
	KindDomain Kind = "domain" // host == value || host 结尾为 .value
	KindSuffix Kind = "suffix" // host 结尾为 .value
	KindCIDR   Kind = "cidr"   // IP 落在 value 网段
)

// Rule 一条分流规则。
type Rule struct {
	Action Action
	Kind   Kind
	Value  string
	net    *net.IPNet // 仅 KindCIDR
}

// Router 按域名/IP 决定直连或代理。顺序匹配，首条命中即返回；都不命中用 default。
type Router struct {
	rules []Rule
	def   Action
}

// New 创建一个 Router，default 为未命中时的动作。
func New(def Action) *Router {
	return &Router{def: def}
}

// validDefault 校验 default 动作。
//
// 先前 parse 把 `default` 后面的词直接当动作收下，不校验 —— 于是 `default proxyy`
// 这种拼错会安静地存进去，Decide 返回 "proxyy"，下游 switch 落到 default 分支当成
// 代理处理。规则文件写错了，行为却照常，只是和你以为的不一样。
func validDefault(a Action) bool {
	switch a {
	case Direct, Proxy, Auto:
		return true
	}
	return false
}

// Add 追加一条规则（会为 cidr 预解析网段）。
func (r *Router) Add(rule Rule) error {
	switch rule.Kind {
	case KindCIDR:
		_, n, err := net.ParseCIDR(rule.Value)
		if err != nil {
			return fmt.Errorf("bad cidr %q: %w", rule.Value, err)
		}
		rule.net = n
	case KindDomain, KindSuffix:
	default:
		return fmt.Errorf("unknown kind %q (supported: domain, suffix, cidr)", rule.Kind)
	}

	switch rule.Action {
	case Direct, Proxy:
	case Auto:
		return fmt.Errorf("auto is only meaningful as the fallback — write `default auto`, " +
			"not a rule with action auto")
	default:
		return fmt.Errorf("unknown action %q (supported: direct, proxy)", rule.Action)
	}

	r.rules = append(r.rules, rule)
	return nil
}

// Decide 对目标主机做分流决策。host 可为域名或 IP 文本，可带端口。
func (r *Router) Decide(rawHost string) Action {
	host := normalizeHost(rawHost)
	if host == "" {
		return r.def
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, rule := range r.rules {
			if rule.Kind != KindCIDR {
				continue
			}
			if rule.net.Contains(ip) {
				return rule.Action
			}
		}
		return r.def
	}
	for _, rule := range r.rules {
		if rule.Kind == KindCIDR {
			continue
		}
		if matchDomain(rule, host) {
			return rule.Action
		}
	}
	return r.def
}

func matchDomain(rule Rule, host string) bool {
	switch rule.Kind {
	case KindDomain:
		return host == rule.Value || strings.HasSuffix(host, "."+rule.Value)
	case KindSuffix:
		return strings.HasSuffix(host, "."+rule.Value)
	}
	return false
}

func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.TrimSuffix(h, ".")
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return strings.TrimSuffix(h[1:len(h)-1], ".")
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.TrimSuffix(host, ".")
	}
	return h
}

// parse 从 reader 读取 netmaster 原生规则格式。
func (r *Router) parse(rd io.Reader) error {
	sc := bufio.NewScanner(rd)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "//") {
			continue
		}
		fields := strings.Fields(text)
		if fields[0] == "default" {
			if len(fields) < 2 {
				return fmt.Errorf("line %d: default needs an action", line)
			}
			def := Action(fields[1])
			if !validDefault(def) {
				return fmt.Errorf("line %d: unknown default action %q "+
					"(supported: direct, proxy, auto)", line, fields[1])
			}
			r.def = def
			continue
		}
		if len(fields) < 3 {
			return fmt.Errorf("line %d: want '<action> <kind> <value>', got %q", line, text)
		}
		rule := Rule{
			Action: Action(fields[0]),
			Kind:   Kind(fields[1]),
			Value:  strings.Join(fields[2:], " "),
		}
		if err := r.Add(rule); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	return sc.Err()
}

// Load 从文件加载规则（netmaster 原生格式），default 为初始默认动作（可被文件内 default 覆盖）。
func Load(path string, def Action) (*Router, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := New(def)
	if err := r.parse(f); err != nil {
		return nil, err
	}
	return r, nil
}
