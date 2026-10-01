package nodepool

import (
	"fmt"
	"net"
	"time"
)

// GeoResolver 判断一个域名解析后是否位于中国大陆。
// 由 geoip.Provider 实现；known=false 表示无法判断（无表或解析失败）。
type GeoResolver interface {
	IsCNHost(host string) (isCN bool, known bool)
}

// Exit 一个出口选择。
type Exit struct {
	Direct bool // true = 本机直连
	Idx    int  // Direct=false 时的节点索引；-1 表示交给自适应挑选
}

// directBlockedTTL 是"该域名禁用直连"的冷却时长。
//
// 冷却而非永久：一次网络抖动不该让这个域名从此失去直连（国内站会因此绕远路）。
const directBlockedTTL = 30 * time.Minute

func (e Exit) String() string {
	switch {
	case e.Direct:
		return "direct"
	case e.Idx < 0:
		return "auto"
	default:
		return fmt.Sprintf("node[%d]", e.Idx)
	}
}

// SetGeo 注入 IP 归属判断器。g 为 nil 时 Auto 一律偏代理。
func (p *Pool) SetGeo(g GeoResolver) { p.geo = g }

// SetDialTimeout 设置直连拨号的超时。
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

// dialDirect 本机直连目标。
func (p *Pool) dialDirect(target string) (net.Conn, error) {
	return net.DialTimeout("tcp", target, p.directTimeout())
}

// PickExit 给出该域名的首选出口（只读，不做网络 I/O）。
//
// 优先级：已学习的绑定 > geoip 先验 > 代理。
//
// 已绑定排在最前，是为了出口 IP 稳定 —— 同一个域名如果一会儿直连（本机宽带 IP）、
// 一会儿代理（CF 边缘或中继 IP），按 IP 判定的登录态会立刻失效。所以绑定一旦建立
// 就粘住，只有它明确失败时才解除重选。
func (p *Pool) PickExit(host string) Exit {
	// 已知这个域名的直连被阻断（代理侧探测到 RST、或直连拨号失败）：别再试直连。
	if p.isDirectBlocked(host) {
		return Exit{Idx: -1}
	}
	if e, ok := p.exitOf(host); ok {
		if e.Direct {
			return e
		}
		if e.Idx >= 0 && e.Idx < p.Len() && !p.states[e.Idx].isDisabled() && !p.latencyExceeded(host, e.Idx) {
			return e
		}
		// 绑定的节点已熔断、或延迟升高超出容忍值 → 落到下面重新判断
	}

	// 没有历史、或历史已失效：用 IP 归属给个先验。
	// 解析失败（known=false）时不猜，按老行为走代理。
	if p.geo != nil {
		if isCN, known := p.geo.IsCNHost(host); known && isCN {
			return Exit{Direct: true}
		}
	}
	return Exit{Idx: -1}
}

// DialAuto 在直连与代理之间选出口建连；首选失败时自动尝试另一条路。
//
// 返回的 direct 表示"这是一次尝试性的直连"：TCP 能连不等于没被墙 —— GFW 常常
// 让 TCP 握手通过，直到看见 TLS ClientHello 里的明文 SNI 才发 RST。代理层若观察到
// 这种情况，应调 RetryProxy 改走代理并重放已缓存的数据。
func (p *Pool) DialAuto(target string) (conn net.Conn, direct bool, err error) {
	host := hostOf(target)
	e := p.PickExit(host)

	if c, derr := p.dialExit(e, target); derr == nil {
		// Idx<0 时由 Dial 自己挑节点并已写好绑定，这里不要用 auto 覆盖掉它。
		if e.Direct || e.Idx >= 0 {
			p.bindExit(host, e)
		}
		return c, e.Direct, nil
	}

	// 首选失败 → 换另一条路。这也是"没有 geoip 表也能自愈"的来源：
	// 猜错了下一跳就纠正过来。
	if e.Direct {
		p.unbindAffinity(host)
		if c, aerr := p.Dial(target); aerr == nil {
			return c, false, nil
		}
	} else {
		// 代理全挂时才试直连（可能这个站没被墙，只是节点都不通）。
		if c, derr := p.dialDirect(target); derr == nil {
			p.bindExit(host, Exit{Direct: true})
			return c, true, nil
		}
	}
	return nil, false, fmt.Errorf("nodepool: no usable exit for %s", target)
}

// dialExit 按出口建连。
func (p *Pool) dialExit(e Exit, target string) (net.Conn, error) {
	if e.Direct {
		return p.dialDirect(target)
	}
	if e.Idx < 0 {
		return p.Dial(target)
	}
	if c, ok := p.tryDial(e.Idx, target); ok {
		return c, nil
	}
	return nil, fmt.Errorf("nodepool: node %d failed for %s", e.Idx, target)
}

// RetryProxy 在直连被判定阻断后改用代理，并记下"这个域名别再走直连"。
func (p *Pool) RetryProxy(target string) (net.Conn, error) {
	host := hostOf(target)
	p.blockDirect(host)
	return p.Dial(target)
}

// blockDirect 记住某域名的直连不可用。
//
// 不记的话会形成级联：代理失败 → 回退直连 → 直连也失败 → 再回代理 → ……
// 每轮都白付一次直连尝试。带 TTL，因为一次网络抖动不该让这个域名永久失去直连。
func (p *Pool) blockDirect(host string) {
	p.affMu.Lock()
	if p.directBlocked == nil {
		p.directBlocked = make(map[string]time.Time)
	}
	p.directBlocked[host] = time.Now()
	delete(p.affinity, host)
	delete(p.bindBase, host)
	p.affMu.Unlock()
}

// isDirectBlocked 判断该域名是否处于"直连被禁用"的冷却期。
func (p *Pool) isDirectBlocked(host string) bool {
	p.affMu.RLock()
	at, ok := p.directBlocked[host]
	p.affMu.RUnlock()
	if !ok {
		return false
	}
	if time.Since(at) > directBlockedTTL {
		p.affMu.Lock()
		delete(p.directBlocked, host)
		p.affMu.Unlock()
		return false
	}
	return true
}

// NoteDirectBlocked 记下"该域名直连被阻断"，下次直接走代理。
func (p *Pool) NoteDirectBlocked(host string) { p.blockDirect(hostOf(host)) }

// ExitFor 返回某域名当前生效的出口（供诊断输出）。
func (p *Pool) ExitFor(host string) string {
	if e, ok := p.exitOf(host); ok {
		return e.String()
	}
	return "-"
}
