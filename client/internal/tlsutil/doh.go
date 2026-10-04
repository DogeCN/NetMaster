// DoH 解析：给"我们自己发起的拨号"一条不被投毒的解析路径。
//
// 为什么需要（2026-10-04 实测）：系统解析器会被投毒 —— `en.wikipedia.org` 解析到
// `31.13.94.41`（Facebook 网段）、`www.bbc.com` 解析到 `108.160.167.30`（Dropbox 网段），
// 拨过去就是黑洞，表现成"TCP 连上、一个字都不回"。而入口候选（服务端域名解析）
// 是整条链路里**唯一没有第三方兜底**的一环：社区优选源全挂时，池子就只剩被污染的
// 结果。这里复用 ECH 那套 IP 直连端点（`dohEndpoints`），所以查询本身没有
// bootstrap 问题（端点里除 dns.alidns.com 外都是字面量 IP）。
//
// 只用于我们**自己发起**的连接（入口候选、ECH 的域名回退）。目标站的直连拨号不走
// 这里：那条路要么是被判直连的 CN 站点（不是投毒目标），要么本来就该走代理。
package tlsutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// dohResolveTimeout 是单次解析查询的上限。
//
// 明显短于 ECH 那条 10s：解析站在启动关键路径上（候选一回来就建池、就绪），
// 而"这次查不到"有系统解析器兜底，不值得为它等十秒。
const dohResolveTimeout = 2 * time.Second

// ResolveIPs 用 DoH 解析域名的 A/AAAA 记录（多端点回退，语义与 ECH 那条一致）。
//
// 返回空切片或错误时，调用方应退回系统解析器：DoH 挂了不比被投毒更糟，行为只增不减。
func ResolveIPs(ctx context.Context, domain, dohBase string) ([]net.IP, error) {
	endpoints := dohEndpoints
	if dohBase != "" {
		endpoints = append([]string{dohBase}, dohEndpoints...)
	}
	var lastErr error
	for _, base := range endpoints {
		ips, err := resolveAtEndpoint(ctx, base, domain)
		if err == nil && len(ips) > 0 {
			return ips, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("doh: no A/AAAA records for %s", domain)
	}
	return nil, lastErr
}

// resolveAtEndpoint 在同一端点并行查 A 与 AAAA：串行会把两个往返叠起来，而它们
// 互不依赖。任一类型成功即可用（IPv6 出站不是所有网络都有）。
func resolveAtEndpoint(ctx context.Context, base, domain string) ([]net.IP, error) {
	type result struct {
		ips []net.IP
		err error
	}
	ch := make(chan result, 2)
	for _, typ := range []string{"A", "AAAA"} {
		go func(typ string) {
			ips, err := dohQueryType(ctx, base, domain, typ)
			ch <- result{ips, err}
		}(typ)
	}
	var out []net.IP
	var firstErr error
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		out = append(out, r.ips...)
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

// dohQueryType 查一种记录类型并解析出 IP。
func dohQueryType(ctx context.Context, base, domain, typ string) ([]net.IP, error) {
	q := fmt.Sprintf("%s?name=%s&type=%s", base, url.QueryEscape(domain), typ)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/dns-json")
	c := &http.Client{Timeout: dohResolveTimeout}
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))

	var resp struct {
		Status int `json:"Status"`
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("doh: bad json from %s: %w", base, err)
	}
	if resp.Status != 0 {
		return nil, fmt.Errorf("doh: %s answered status %d", base, resp.Status)
	}
	var out []net.IP
	for _, a := range resp.Answer {
		// 只认地址记录：CNAME 链里的名字不是 IP，解析出来也不能拨。
		if a.Type != 1 && a.Type != 28 {
			continue
		}
		if ip := net.ParseIP(a.Data); ip != nil {
			out = append(out, ip)
		}
	}
	return out, nil
}
