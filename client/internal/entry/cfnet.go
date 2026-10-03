package entry

import (
	"net/netip"
)

// Cloudflare 公布的 IPv4 网段（cloudflare.com/ips，与 server/src/exits.js 的 CF_V4
// 同源）。客户端只能用**优选 IP 直连**，而优选 IP 必须落在这些网段里 —— 落在外面的
// 条目根本不是承载自定义域的边缘地址，拨过去必然失败。
//
// 为什么放在客户端而不是共用 server 的模块：两端不共享代码（server 是 ESM bundle、
// client 是 Go module），复制一份常量比跨语言依赖便宜；改动时两边同步即可。
var cfV4 = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
}

var cfNets = func() []*netip.Prefix {
	out := make([]*netip.Prefix, 0, len(cfV4))
	for _, c := range cfV4 {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			continue
		}
		out = append(out, &p)
	}
	return out
}()

// IsCloudflareIP 报告该地址是否落在 Cloudflare 公布的 IPv4 网段内。
// 解析失败或非 IPv4 一律 false —— 候选源里出现主机名时宁可不收。
func IsCloudflareIP(addr string) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil || !a.Is4() {
		return false
	}
	for _, p := range cfNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// FilterCloudflare 剔掉不在 Cloudflare 网段内的候选。
//
// 实测依据（协助者 2026-10-03，三源各拉两次）：`090227.pages.dev` 返回的 150 条里
// 有 **28 条（19%）** 不在 CF 官方网段内，样例能看到 `188.164.248.x`、`8.35.211.169`
// （后者属 Google 段）。这些条目作为入口必然无效，却会占用 maxEntries 的名额并白烧
// 探测预算。
//
// ⚠️ 更正记录：协助者第一版测量用的是偏宽的自造网段表（含 `104.16.0.0/12`、
// `199.27.128.0/17`、`23.10.0.0/15` 等**非官方段**），得出"15 条 / 10%"。
// 换成 cloudflare.com/ips 官方 15 条后是 **28 条 / 19%**。别再引用旧数字。
//
// 另外注意：**这个列表是动态的** —— 同一个源在相隔几分钟的两次调用里返回的 IP 集合
// 已经不同（它是"实时优选"服务）。所以上面的比例是快照，不是固定基线。
//
// 保留无法解析的条目（主机名形式）：源偶尔会给域名，那一条交给 TLS 探测去判，
// 这里不越权把它毙掉。
func FilterCloudflare(nodes []Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if _, err := netip.ParseAddr(n.Addr); err == nil && !IsCloudflareIP(n.Addr) {
			continue
		}
		out = append(out, n)
	}
	return out
}
