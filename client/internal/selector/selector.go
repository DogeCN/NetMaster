// Package selector 管理出口节点池：每节点一条复用的 MuxConn，按域名决定
// 直连或代理（M1 精简版：geoip 先验 + 直连阻断冷却；PRD §6.3 的规则分流与
// 延迟优选在 M4 由 internal/rules 与本包的完整版替代）。
package selector

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"netmaster/internal/entry"
	"netmaster/internal/outbound"
)

// GeoResolver 判断一个域名解析后是否位于中国大陆。
// 由 geoip.Provider 实现；known=false 表示无法判断（无表或解析失败）。
type GeoResolver interface {
	IsCNHost(host string) (isCN bool, known bool)
}

type Config struct {
	Nodes    []entry.Node
	SNI      string
	Password string
	UseECH   bool
	Insecure bool
}

// directBlockedTTL 是"该域名禁用直连"的冷却时长。
// 冷却而非永久：一次网络抖动不该让这个域名从此失去直连（国内站会因此绕远路）。
const directBlockedTTL = 30 * time.Minute

type Pool struct {
	nodes    []entry.Node
	sni      string
	password string
	useECH   bool
	insecure bool

	dialTimeout time.Duration // 直连拨号超时；0 = 默认 15s
	geo         GeoResolver

	mu            sync.Mutex
	muxes         map[int]*outbound.MuxConn // 节点索引 -> 活跃 mux
	directBlocked map[string]time.Time
	direct        map[string]bool // host -> 已学习的直连/代理粘性
}

func New(cfg Config) *Pool {
	return &Pool{
		nodes:         cfg.Nodes,
		sni:           cfg.SNI,
		password:      cfg.Password,
		useECH:        cfg.UseECH,
		insecure:      cfg.Insecure,
		muxes:         make(map[int]*outbound.MuxConn),
		directBlocked: make(map[string]time.Time),
		direct:        make(map[string]bool),
	}
}

func (p *Pool) SetGeo(g GeoResolver)    { p.geo = g }
func (p *Pool) SetDialTimeout(d time.Duration) {
	if d > 0 {
		p.dialTimeout = d
	}
}
func (p *Pool) directTimeout() time.Duration {
	if p.dialTimeout <= 0 {
		return 15 * time.Second
	}
	return p.dialTimeout
}

func (p *Pool) Len() int { return len(p.nodes) }

func hostOf(target string) string {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return target
	}
	return host
}

// Dial 经代理出口建连到 target。在节点池里随机起点轮询，节点 mux 失效时
// 重拨一次；整池失败返回最后一个错误。
func (p *Pool) Dial(target string) (net.Conn, error) {
	p.mu.Lock()
	order := rand.Perm(len(p.nodes))
	muxes := make(map[int]*outbound.MuxConn, len(p.muxes))
	for k, v := range p.muxes {
		muxes[k] = v
	}
	p.mu.Unlock()

	var lastErr error
	for _, idx := range order {
		m, ok := muxes[idx]
		if ok && m.Alive() {
			conn, err := m.Open(target)
			if err == nil {
				return conn, nil
			}
			lastErr = err
			// 流被拒（0x01-0x03）是服务端裁决，换节点也是同一裁决，不再轮询。
			if m.Alive() {
				return nil, lastErr
			}
		}
		// mux 不存在或已死：重拨这条传输，再试一次流。
		conn, err := p.dialNode(idx, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("selector: node pool is empty")
	}
	return nil, lastErr
}

// dialNode 建立节点 idx 的 mux 并在其上打开 target。
func (p *Pool) dialNode(idx int, target string) (net.Conn, error) {
	p.mu.Lock()
	m, ok := p.muxes[idx]
	p.mu.Unlock()
	if ok && m.Alive() {
		if conn, err := m.Open(target); err == nil {
			return conn, nil
		}
	}
	client := &outbound.Client{
		Node:     p.nodes[idx],
		SNI:      p.sni,
		Password: p.password,
		UseECH:   p.useECH,
		Insecure: p.insecure,
	}
	m, err := outbound.DialMux(client)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.muxes[idx] = m
	p.mu.Unlock()
	conn, err := m.Open(target)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// shouldDirect 给出该域名的直连先验：已学习的粘性 > geoip 判断（PRD：出口 IP
// 稳定——同域一会儿直连一会儿代理会让按 IP 判定的登录态失效）。
func (p *Pool) shouldDirect(host string) bool {
	p.mu.Lock()
	sticky, ok := p.direct[host]
	blocked := false
	if at, has := p.directBlocked[host]; has {
		if time.Since(at) > directBlockedTTL {
			delete(p.directBlocked, host)
		} else {
			blocked = true
		}
	}
	p.mu.Unlock()
	if blocked {
		return false
	}
	if ok {
		return sticky
	}
	if p.geo != nil {
		if isCN, known := p.geo.IsCNHost(host); known && isCN {
			return true
		}
	}
	return false
}

func (p *Pool) bindDirect(host string, direct bool) {
	p.mu.Lock()
	p.direct[host] = direct
	p.mu.Unlock()
}

func (p *Pool) unbind(host string) {
	p.mu.Lock()
	delete(p.direct, host)
	p.mu.Unlock()
}

// DialAuto 在直连与代理之间选出口建连；首选失败时自动尝试另一条路。
// 返回的 direct 表示"这是一次尝试性的直连"：TCP 能连不等于没被墙 —— GFW 常常
// 让 TCP 握手通过，直到看见 TLS ClientHello 里的明文 SNI 才发 RST。代理层若观察
// 到这种情况，应调 RetryProxy 改走代理并重放已缓存的数据。
func (p *Pool) DialAuto(target string) (net.Conn, bool, error) {
	host := hostOf(target)
	wantDirect := p.shouldDirect(host)

	if wantDirect {
		if c, err := net.DialTimeout("tcp", target, p.directTimeout()); err == nil {
			p.bindDirect(host, true)
			return c, true, nil
		}
		p.unbind(host)
		if c, err := p.Dial(target); err == nil {
			return c, false, nil
		}
		return nil, false, fmt.Errorf("selector: no usable exit for %s", target)
	}

	if c, err := p.Dial(target); err == nil {
		return c, false, nil
	}
	// 代理全挂时才试直连（可能这个站没被墙，只是节点都不通）。
	if c, err := net.DialTimeout("tcp", target, p.directTimeout()); err == nil {
		p.bindDirect(host, true)
		return c, true, nil
	}
	return nil, false, fmt.Errorf("selector: no usable exit for %s", target)
}

// RetryProxy 在直连被判定阻断后改用代理，并记下"这个域名别再走直连"。
func (p *Pool) RetryProxy(target string) (net.Conn, error) {
	host := hostOf(target)
	p.mu.Lock()
	p.directBlocked[host] = time.Now()
	p.mu.Unlock()
	p.unbind(host)
	return p.Dial(target)
}

// NoteProxyFailure 记录某目标经代理失败。M1 为占位（M4 的节点剔除接管）。
func (p *Pool) NoteProxyFailure(target string) {}

// Verify 建立一次真实的传输层验证（TLS+WS+首帧认证）。部署是否健康，
// 这一条是最直接的回答。
func (p *Pool) Verify() (string, error) {
	if len(p.nodes) == 0 {
		return "", fmt.Errorf("selector: no nodes")
	}
	// 首流目标是 example.com:80：非 CF 托管（直连不会被平台 CF 段规则拒绝），
	// 建流成功即同时验证了传输层（TLS+WS+认证）与出站路径。
	_, err := p.dialNode(0, "example.com:80")
	if err != nil {
		return "", err
	}
	return p.nodes[0].Addr, nil
}

// Nodes 返回节点列表（诊断用）。
func (p *Pool) Nodes() []entry.Node { return p.nodes }
