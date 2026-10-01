// Package geoip 判断一个主机是否在中国大陆。
//
// 分流不能只看域名字符串：域名列表无法穷举，未列出的站点只能猜，猜错就把
// "国内 IP、没被墙"的站点绕到海外再绕回来。所以解析目标域名，落在 CN 网段
// 就直连。
//
// 表从外部拉取并落盘缓存（约 4300 条 CIDR）。没表时所有查询都返回 known=false，
// 调用方据此退回"一律偏代理"，不会因为拉取失败而影响可用性。
package geoip

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"netmaster/internal/cache"
)

// DefaultURL 是 CN 网段表来源。国内直连可达（相比 raw.githubusercontent.com
// 那类 CF 承载的源，在这里更稳）。
const DefaultURL = "https://ispip.clang.cn/all_cn.txt"

// DefaultTTL 是表的缓存有效期。CN 网段变化很慢，一周足够。
const DefaultTTL = 7 * 24 * time.Hour

const (
	cacheNamespace = "geoip"
	// dnsTTL 是域名→是否 CN 的缓存时间。IP 归属几乎不变，缓存久一点没关系，
	// 主要是避免同一域名反复解析（尤其在代理每条连接都要判断的情况下）。
	dnsTTL = 30 * time.Minute
	// dnsTimeout 限制单次解析，避免系统 DNS 卡住拖慢建连。
	dnsTimeout = 3 * time.Second
	// lookupCacheMax 是解析结果缓存的条数上限。
	lookupCacheMax = 4096
)

// cidrRange 是一个已合并的地址区间（闭区间）。
type cidrRange struct{ start, end uint32 }

// Table 是排好序、已合并的 CN 网段集合，用二分查找判归属。
type Table struct {
	ranges []cidrRange
}

// Len 返回合并后的网段数（用于诊断）。
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.ranges)
}

// Parse 解析 CIDR 文本（每行一条，`#` 开头与空行忽略）。非 IPv4 的行跳过。
func Parse(r io.Reader) (*Table, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var rs []cidrRange
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 有的表带行尾注释
		if i := strings.IndexAny(line, " \t#"); i >= 0 {
			line = line[:i]
		}
		_, ipnet, err := net.ParseCIDR(line)
		if err != nil {
			continue
		}
		v4 := ipnet.IP.To4()
		ones, bits := ipnet.Mask.Size()
		if v4 == nil || bits != 32 {
			continue // 表里只有 IPv4
		}
		start := binary.BigEndian.Uint32(v4)
		var mask uint32
		if ones > 0 {
			mask = ^uint32(0) << (32 - ones)
		}
		rs = append(rs, cidrRange{start: start, end: start | ^mask})
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("geoip: no usable CIDR in %d bytes", len(b))
	}
	return &Table{ranges: mergeRanges(rs)}, nil
}

// mergeRanges 排序并合并重叠/相邻区间，让二分查找可以假定区间互不相交。
func mergeRanges(rs []cidrRange) []cidrRange {
	sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		// 相邻（last.end+1 == r.start）也合并，减少区间数。
		if r.start <= last.end || (last.end != ^uint32(0) && r.start == last.end+1) {
			if r.end > last.end {
				last.end = r.end
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// Contains 判断 IP 是否落在 CN 网段内。表只有 IPv4，纯 IPv6 一律返回 false。
func (t *Table) Contains(ip net.IP) bool {
	if t == nil || len(t.ranges) == 0 {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	n := binary.BigEndian.Uint32(v4)
	i := sort.Search(len(t.ranges), func(i int) bool { return t.ranges[i].end >= n })
	return i < len(t.ranges) && t.ranges[i].start <= n
}

type lookupEntry struct {
	isCN    bool
	known   bool
	expires time.Time
}

// Provider 持有网段表，并提供"这个主机是否在中国大陆"的判断（自带解析缓存）。
type Provider struct {
	url    string
	ttl    time.Duration
	client *http.Client

	mu       sync.RWMutex
	table    *Table
	loadedAt time.Time

	lookupMu sync.Mutex
	lookups  map[string]lookupEntry
}

// New 创建 Provider。url 为空时用 DefaultURL；ttl<=0 时用 DefaultTTL。
func New(url string, ttl time.Duration) *Provider {
	if url == "" {
		url = DefaultURL
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Provider{
		url:     url,
		ttl:     ttl,
		client:  &http.Client{Timeout: 30 * time.Second},
		lookups: make(map[string]lookupEntry),
	}
}

// Ready 报告表是否已可用。
func (p *Provider) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.table != nil
}

// Size 返回当前表的网段数。
func (p *Provider) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.table.Len()
}

// Age 返回表的年龄；未加载时返回 -1。
func (p *Provider) Age() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.table == nil {
		return -1
	}
	return time.Since(p.loadedAt)
}

type cacheFile struct {
	URL  string `json:"url"`
	Body string `json:"body"`
}

// loadCache 尝试用磁盘缓存装表。返回是否装上、以及缓存是否还新鲜。
func (p *Provider) loadCache() (loaded, fresh bool) {
	var c cacheFile
	age, ok := cache.Load(cacheNamespace, p.url, &c)
	if !ok || c.Body == "" || c.URL != p.url {
		return false, false
	}
	t, err := Parse(strings.NewReader(c.Body))
	if err != nil {
		return false, false
	}
	p.mu.Lock()
	p.table = t
	// 用缓存年龄回推加载时间，这样"是否过期"的判断跨进程也成立。
	p.loadedAt = time.Now().Add(-age)
	p.mu.Unlock()
	return true, age < p.ttl
}

// Refresh 联网拉取并写缓存。
func (p *Provider) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", p.url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("geoip: %s -> HTTP %d", p.url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	t, err := Parse(strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.table = t
	p.loadedAt = time.Now()
	p.mu.Unlock()
	_ = cache.Save(cacheNamespace, p.url, cacheFile{URL: p.url, Body: string(body)})
	p.clearLookups()
	return nil
}

func (p *Provider) clearLookups() {
	p.lookupMu.Lock()
	p.lookups = make(map[string]lookupEntry)
	p.lookupMu.Unlock()
}

// Start 先装缓存，再在后台按需刷新。
//
// 不阻塞启动：没表时 IsCNHost 返回 known=false，调用方会退回原行为；
// 表在后台装好后自动生效。
func (p *Provider) Start(onReady func(size int, fromCache bool)) {
	loaded, fresh := p.loadCache()
	if loaded && onReady != nil {
		onReady(p.Size(), true)
	}
	if loaded && fresh {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := p.Refresh(ctx); err != nil {
			// 拉取失败不影响可用性：要么继续用旧表，要么退回原行为。
			return
		}
		if onReady != nil {
			onReady(p.Size(), false)
		}
	}()
}

// IsCNHost 判断主机是否在中国大陆。
//
// known=false 表示"没表或解析失败"，即无法判断——调用方应退回原行为，
// 而不是把它当成"不在中国"。
func (p *Provider) IsCNHost(host string) (isCN bool, known bool) {
	if !p.Ready() {
		return false, false
	}
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// IP 字面量直接查表，不用解析。
	if ip := net.ParseIP(host); ip != nil {
		p.mu.RLock()
		t := p.table
		p.mu.RUnlock()
		return t.Contains(ip), true
	}

	now := time.Now()
	p.lookupMu.Lock()
	if e, ok := p.lookups[host]; ok && now.Before(e.expires) {
		p.lookupMu.Unlock()
		return e.isCN, e.known
	}
	p.lookupMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		// 解析失败不缓存：临时故障不该被记成"非 CN"。
		return false, false
	}
	p.mu.RLock()
	t := p.table
	p.mu.RUnlock()
	cn := false
	for _, ip := range ips {
		if t.Contains(ip) {
			cn = true
			break
		}
	}

	p.lookupMu.Lock()
	if len(p.lookups) >= lookupCacheMax {
		p.lookups = make(map[string]lookupEntry)
	}
	p.lookups[host] = lookupEntry{isCN: cn, known: true, expires: now.Add(dnsTTL)}
	p.lookupMu.Unlock()
	return cn, true
}
