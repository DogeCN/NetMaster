package rules

import (
	"strings"
)

// Ruleset 一份待解析的规则列表：来源名 + 正文 + 整份列表的动作。
type Ruleset struct {
	Name   string
	Body   string
	Action Action
}

// ParseStats 一次解析的统计，供调用方记日志。
type ParseStats struct {
	Name    string
	Skipped int // 认不出来或格式不对的行数
	Total   int // 非空、非注释的行数
}

// ParseClashRuleset 解析一份 Clash RULE-SET 文本，整份列表统一用 action。
//
// 认这些行：DOMAIN / DOMAIN-SUFFIX / DOMAIN-KEYWORD / IP-CIDR / IP-CIDR6
// （含 IP-CIDR 的 ,no-resolve 尾注）。`#` 注释与空行忽略；认不出来的行跳过
// 并计数 —— 第三方列表里混着 GEOSITE、PROCESS-NAME、MATCH 之类我们不支持的
// 类型是常态，因为一行不认识就让客户端起不来不可接受。
func ParseClashRuleset(name, body string, action Action) ([]Rule, ParseStats) {
	if action != Direct && action != Proxy {
		action = Proxy
	}
	st := ParseStats{Name: name}
	var out []Rule
	seen := make(map[Rule]struct{})

	appendRule := func(kind Kind, value string) {
		value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
		if value == "" {
			st.Skipped++
			return
		}
		rule := Rule{Kind: kind, Value: value, Action: action}
		if _, dup := seen[rule]; dup {
			return // 同一份列表里重复很常见，去重省下索引空间
		}
		seen[rule] = struct{}{}
		out = append(out, rule)
	}

	for _, line := range strings.Split(body, "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "//") {
			continue
		}
		st.Total++
		fields := strings.Split(text, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 2 || fields[1] == "" {
			st.Skipped++
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "DOMAIN":
			appendRule(KindExact, fields[1])
		case "DOMAIN-SUFFIX":
			appendRule(KindSuffix, fields[1])
		case "DOMAIN-KEYWORD":
			appendRule(KindKeyword, fields[1])
		case "IP-CIDR", "IP-CIDR6":
			// 尾注 no-resolve 忽略：客户端不预解析域名，用不上这个标记。
			appendRule(KindCIDR, fields[1])
		default:
			st.Skipped++
		}
	}
	return out, st
}

// BuiltinRulesets 兜底规则集：内置源拉不到、缓存也没有时用。
//
// 只写两件事：私网/回环（服务端连不上，必须直连）和最常见的需要代理的域名
// 后缀。故意做小 —— 它是"完全没网络时的最低可用"，不是一份完整列表；其余
// 站点靠 GeoIP 判断，判断不出来走兜底动作。
func BuiltinRulesets() []Ruleset {
	return []Ruleset{
		{Name: "private", Body: builtinPrivate, Action: Direct},
		{Name: "gfw", Body: builtinGfw, Action: Proxy},
	}
}

const builtinPrivate = `# netmaster builtin: private / loopback (direct)
IP-CIDR,0.0.0.0/8
IP-CIDR,10.0.0.0/8
IP-CIDR,100.64.0.0/10
IP-CIDR,127.0.0.0/8
IP-CIDR,169.254.0.0/16
IP-CIDR,172.16.0.0/12
IP-CIDR,192.168.0.0/16
IP-CIDR,198.18.0.0/15
IP-CIDR6,::1/128
IP-CIDR6,fc00::/7
IP-CIDR6,fe80::/10
DOMAIN,localhost
DOMAIN-SUFFIX,local
`

const builtinGfw = `# netmaster builtin: commonly blocked domains (proxy)
DOMAIN-SUFFIX,google.com
DOMAIN-SUFFIX,googleapis.com
DOMAIN-SUFFIX,googlevideo.com
DOMAIN-SUFFIX,gstatic.com
DOMAIN-SUFFIX,ggpht.com
DOMAIN-SUFFIX,blogspot.com
DOMAIN-SUFFIX,youtube.com
DOMAIN-SUFFIX,youtu.be
DOMAIN-SUFFIX,ytimg.com
DOMAIN-SUFFIX,gmail.com
DOMAIN-SUFFIX,googlemail.com
DOMAIN-SUFFIX,facebook.com
DOMAIN-SUFFIX,fbcdn.net
DOMAIN-SUFFIX,instagram.com
DOMAIN-SUFFIX,whatsapp.com
DOMAIN-SUFFIX,telegram.org
DOMAIN-SUFFIX,t.me
DOMAIN-SUFFIX,twitter.com
DOMAIN-SUFFIX,twimg.com
DOMAIN-SUFFIX,x.com
DOMAIN-SUFFIX,linkedin.com
DOMAIN-SUFFIX,reddit.com
DOMAIN-SUFFIX,redd.it
DOMAIN-SUFFIX,discord.com
DOMAIN-SUFFIX,discordapp.com
DOMAIN-SUFFIX,twitch.tv
DOMAIN-SUFFIX,netflix.com
DOMAIN-SUFFIX,nflxvideo.net
DOMAIN-SUFFIX,spotify.com
DOMAIN-SUFFIX,wikipedia.org
DOMAIN-SUFFIX,wikimedia.org
DOMAIN-SUFFIX,medium.com
DOMAIN-SUFFIX,github.com
DOMAIN-SUFFIX,githubusercontent.com
DOMAIN-SUFFIX,githubassets.com
DOMAIN-SUFFIX,github.io
DOMAIN-SUFFIX,gitlab.com
DOMAIN-SUFFIX,archive.org
DOMAIN-SUFFIX,bbc.com
DOMAIN-SUFFIX,bbc.co.uk
DOMAIN-SUFFIX,nytimes.com
DOMAIN-SUFFIX,wsj.com
DOMAIN-SUFFIX,economist.com
DOMAIN-SUFFIX,ft.com
DOMAIN-SUFFIX,reuters.com
DOMAIN-SUFFIX,apnews.com
DOMAIN-SUFFIX,openai.com
DOMAIN-SUFFIX,claude.ai
DOMAIN-SUFFIX,anthropic.com
DOMAIN-SUFFIX,steamcommunity.com
DOMAIN-SUFFIX,steampowered.com
DOMAIN-SUFFIX,pixiv.net
DOMAIN-SUFFIX,pximg.net
DOMAIN-SUFFIX,nintendo.com
`
