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
	fails         map[int]int     // 节点索引 -> 连续失败次数
	dead          map[int]bool    // 连续失败到阈值判死，等待重随机时复活
	pending       *dialPending    // 断线等待队列与退避重连
}

func New(cfg Config) *Pool {
	p := &Pool{
		nodes:         cfg.Nodes,
		sni:           cfg.SNI,
		password:      cfg.Password,
		useECH:        cfg.UseECH,
		insecure:      cfg.Insecure,
		muxes:         make(map[int]*outbound.MuxConn),
		directBlocked: make(map[string]time.Time),
		direct:        make(map[string]bool),
		fails:         make(map[int]int),
		dead:          make(map[int]bool),
	}
	p.pending = newDialPending(p)
	return p
}

// Close 停止等待队列并关闭所有传输（进程退出 / 测试收尾）。
func (p *Pool) Close() {
	p.pending.stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.muxes {
		m.Close()
	}
	p.muxes = make(map[int]*outbound.MuxConn)
}

// liveMux 返回任一存活传输及其节点索引（idx = -1 表示全灭）。
func (p *Pool) liveMux() (int, *outbound.MuxConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for idx, m := range p.muxes {
		if m.Alive() {
			return idx, m
		}
	}
	return -1, nil
}

// redial 重建一条传输；成功返回 true（失败时 mux 不入表）。
func (p *Pool) redial() bool {
	idx := p.pickNode()
	if idx < 0 {
		return false
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
		return false
	}
	p.mu.Lock()
	p.muxes[idx] = m
	p.mu.Unlock()
	return true
}

// pickNode 随机选一个未判死的节点；全判死时全部复活（池耗尽后仍要能继续用）。
func (p *Pool) pickNode() int {
	p.mu.Lock()
	live := make([]int, 0, len(p.nodes))
	for i := range p.nodes {
		if !p.dead[i] {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		for i := range p.nodes {
			p.dead[i] = false
			p.fails[i] = 0
			live = append(live, i)
		}
	}
	p.mu.Unlock()
	if len(live) == 0 {
		return -1
	}
	return live[rand.Intn(len(live))]
}

// noteFailure 记一次节点失败；到阈值判死（退避与重随机由 pickNode 接管）。
func (p *Pool) noteFailure(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails[idx]++
	if p.fails[idx] >= nodeFailLimit {
		p.dead[idx] = true
		p.fails[idx] = 0
		delete(p.muxes, idx)
	}
}

// noteSuccess 清零某节点的失败计数。
func (p *Pool) noteSuccess(idx int) {
	p.mu.Lock()
	if p.fails[idx] > 0 {
		p.fails[idx] = 0
	}
	p.mu.Unlock()
}

func (p *Pool) SetGeo(g GeoResolver) { p.geo = g }
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

// Dial 经代理出口建连到 target。
//
// 快路径：任一存活传输直接开流。传输全灭（或建流途中断链）时不立刻把失败
// 丢给用户，而是排进等待队列等重连 —— 断线瞬间上层往往正有一批连接在建立。
func (p *Pool) Dial(target string) (net.Conn, error) {
	if len(p.nodes) == 0 {
		return nil, errNoUsableExit
	}
	for attempt := 0; attempt < 2; attempt++ {
		idx, m := p.liveMux()
		if m != nil {
			conn, err := m.Open(target)
			if err == nil {
				p.noteSuccess(idx)
				return conn, nil
			}
			if m.Alive() {
				// 流被拒（0x01-0x03）是服务端裁决，换节点也是同一裁决。
				p.noteFailure(idx)
				return nil, err
			}
			// 传输在半路死了：记一次失败（到阈值判死）并重建
			p.noteFailure(idx)
			if !p.redial() {
				break
			}
			continue
		}
		// 没有可用传输：当场重建一条
		if !p.redial() {
			break
		}
	}
	// 仍然不可用 → 排队等待（带退避）
	req, err := p.pending.enqueue(target)
	if err != nil {
		return nil, err
	}
	if err := p.pending.wait(req); err != nil {
		return nil, err
	}
	if _, m := p.liveMux(); m != nil {
		return m.Open(target)
	}
	return nil, errNoUsableExit
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

// NoteProxyFailure 记录某目标经代理失败：立刻重随机换节点（PRD §6.7）。
func (p *Pool) NoteProxyFailure(target string) {
	p.mu.Lock()
	// 断开当前传输，下一次 Dial 就会重建到别的节点。
	for idx, m := range p.muxes {
		p.noteFailureLocked(idx)
		m.Close()
		delete(p.muxes, idx)
	}
	p.mu.Unlock()
}

// noteFailureLocked 是 noteFailure 的加锁版本。
func (p *Pool) noteFailureLocked(idx int) {
	p.fails[idx]++
	if p.fails[idx] >= nodeFailLimit {
		p.dead[idx] = true
		p.fails[idx] = 0
	}
}

// Verify 建立一次真实的传输层验证（TLS+WS+首帧认证）。部署是否健康，
// 这一条是最直接的回答。
func (p *Pool) Verify() (string, error) {
	if len(p.nodes) == 0 {
		return "", fmt.Errorf("selector: no nodes")
	}
	// 首流目标是 www.google.com:443：M0 实测（m0-findings.md E6/E7）平台禁拨 80
	// 端口、example.com 已迁 CF 网段，两者任占一条这个验证都会被出口层拒绝。
	// 443 + 非 CF 目标是直连路径上唯一稳定的组合；建流成功即同时验证了
	// 传输层（TLS+WS+认证）与出站路径。
	_, err := p.dialNode(0, "www.google.com:443")
	if err != nil {
		return "", err
	}
	return p.nodes[0].Addr, nil
}

// Nodes 返回节点列表（诊断用）。
func (p *Pool) Nodes() []entry.Node { return p.nodes }
