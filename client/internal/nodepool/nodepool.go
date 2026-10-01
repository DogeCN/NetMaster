package nodepool

import (
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"netmaster/internal/cache"
	"netmaster/internal/entry"
	"netmaster/internal/outbound"
)

// nodeState 记录单个节点的运行时健康度与延迟，供自适应选择使用。
type nodeState struct {
	mu          sync.Mutex
	latency     time.Duration // 最近一次成功拨号的耗时（EWMA）
	success     int           // 累计成功次数
	fail        int           // 累计失败次数
	consecutive int           // 连续失败次数
	disabled    bool          // 连续失败过多，暂时熔断
}

// score 综合评分，越小越好。用于在多个候选节点间挑选。
// 优先：健康（未熔断） > 成功率 > 实测延迟。带轻微随机抖动避免羊群效应。
func (s *nodeState) score(rng *rand.Rand) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return 1e9
	}
	total := s.success + s.fail
	succRate := 1.0
	if total > 0 {
		succRate = float64(s.success) / float64(total)
	}
	lat := float64(s.latency.Microseconds()) / 1000.0 // ms
	if lat <= 0 {
		lat = 500 // 未测到延迟给个中性值
	}
	// 成功率权重高，延迟次之；连续失败额外惩罚。
	v := (1-succRate)*1000 + lat + float64(s.consecutive)*200
	// 抖动：±15%，避免所有连接同时压向同一节点
	v *= 0.85 + rng.Float64()*0.3
	return v
}

func (s *nodeState) recordSuccess(d time.Duration) {
	s.mu.Lock()
	s.success++
	s.consecutive = 0
	if s.latency == 0 {
		s.latency = d
	} else {
		// EWMA：旧值 70%，新值 30%，平滑抖动
		s.latency = time.Duration(float64(s.latency)*0.7 + float64(d)*0.3)
	}
	s.mu.Unlock()
}

func (s *nodeState) recordFailure() {
	s.mu.Lock()
	s.fail++
	s.consecutive++
	// 连续失败 3 次熔断，稍后由探测恢复
	if s.consecutive >= 3 {
		s.disabled = true
	}
	s.mu.Unlock()
}

func (s *nodeState) isDisabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabled
}

// Config 是 Pool 的全部配置。
type Config struct {
	Nodes    []entry.Node
	SNI      string // 服务端域名：TLS SNI、WS Host、ECH 内层名字
	Auth     []byte // 16 字节，md5(utf8(PASSWORD))
	UseECH   bool
	Insecure bool
	// StateKey 亲和与直连冷却的持久化标识（用服务端域名）。换服务端时旧状态
	// 天然失效；为空则不持久化（仅诊断命令用）。
	StateKey string
}

// Pool 持有一个入口候选列表，支持自适应选择、故障转移与延迟排序。
type Pool struct {
	nodes    []entry.Node
	states   []*nodeState
	sni      string
	auth     []byte
	useECH   bool
	insecure bool

	idx   uint32 // 当前主节点（sticky 优先）
	rng   *rand.Rand
	rngMu sync.Mutex

	// affinity 是"域名 → 出口"的唯一映射：保证同一域名始终走同一出口
	// （同站出口 IP 稳定 → 登录态友好），不同域名各自选优。
	// 出口既可以是某个节点，也可以是本机直连（见 exit.go）。
	affMu    sync.RWMutex
	affinity map[string]Exit

	// geo 判断域名是否解析到 CN 网段，用于给"未明确分流"的主机挑首次出口。
	// 可为 nil —— 此时 Auto 一律偏代理。
	geo GeoResolver
	// directBlocked 记录"这个域名的直连不可用"及其时间，用于冷却期内跳过直连。
	// 受 affMu 保护。
	directBlocked map[string]time.Time
	// dialTimeout 是直连拨号的超时。
	dialTimeout time.Duration

	// 学到的状态（affinity/directBlocked）落盘：学到的最有价值的知识不该
	// 活不过一次重启。脏标记 + 防抖，避免每个新域名都同步写盘。
	stateKey    string
	saveMu      sync.Mutex
	savePending bool

	// mux 连接池：每个节点复用一条 WS 传输，上面并发跑多个会话，
	// 避免每请求都做 TCP+TLS(ECH)+WS 握手。mux 是唯一传输路径。
	muxMu    sync.Mutex
	muxConns map[int][]*outbound.MuxConn // nodeIdx -> 空闲 mux 连接
}

// New 创建节点池。
func New(cfg Config) *Pool {
	states := make([]*nodeState, len(cfg.Nodes))
	for i := range states {
		states[i] = &nodeState{}
	}
	p := &Pool{
		nodes:         cfg.Nodes,
		states:        states,
		sni:           cfg.SNI,
		auth:          cfg.Auth,
		useECH:        cfg.UseECH,
		insecure:      cfg.Insecure,
		affinity:      make(map[string]Exit),
		directBlocked: make(map[string]time.Time),
		stateKey:      cfg.StateKey,
		muxConns:      make(map[int][]*outbound.MuxConn),
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	p.loadState()
	return p
}

func (p *Pool) takeMux(idx int) *outbound.MuxConn {
	p.muxMu.Lock()
	defer p.muxMu.Unlock()
	q := p.muxConns[idx]
	if len(q) == 0 {
		return nil
	}
	m := q[len(q)-1]
	p.muxConns[idx] = q[:len(q)-1]
	return m
}

func (p *Pool) putMux(idx int, m *outbound.MuxConn) {
	p.muxMu.Lock()
	defer p.muxMu.Unlock()
	p.muxConns[idx] = append(p.muxConns[idx], m)
}

// MuxStats 返回每个节点的 mux 连接数与活跃会话数（诊断用）。
func (p *Pool) MuxStats() []string {
	p.muxMu.Lock()
	defer p.muxMu.Unlock()
	out := make([]string, 0, len(p.nodes))
	for i := range p.nodes {
		conns := p.muxConns[i]
		total := 0
		for _, c := range conns {
			total += c.LiveCount()
		}
		out = append(out, fmt.Sprintf("[%d] %s muxConns=%d liveSessions=%d",
			i, p.nodes[i].Name, len(conns), total))
	}
	return out
}

// affinityOf 返回该域名绑定的节点索引；未绑定、或绑定的是直连时返回 -1。
func (p *Pool) affinityOf(host string) int {
	e, ok := p.exitOf(host)
	if !ok || e.Direct {
		return -1
	}
	return e.Idx
}

// exitOf 返回该域名绑定的出口。
func (p *Pool) exitOf(host string) (Exit, bool) {
	p.affMu.RLock()
	defer p.affMu.RUnlock()
	e, ok := p.affinity[host]
	return e, ok
}

// bindExit 把域名绑定到出口。限制表大小，超出时随机淘汰一部分（避免无限增长）。
func (p *Pool) bindExit(host string, e Exit) {
	p.affMu.Lock()
	p.affinity[host] = e
	if len(p.affinity) > 4096 {
		// 简单随机淘汰一半
		n := 0
		for k := range p.affinity {
			delete(p.affinity, k)
			n++
			if n > 2048 {
				break
			}
		}
		p.affinity[host] = e // 保住当前绑定
	}
	p.affMu.Unlock()
	p.markStateDirty()
}

// bindAffinity 把域名绑定到节点。
func (p *Pool) bindAffinity(host string, idx int) {
	p.bindExit(host, Exit{Idx: idx})
}

// unbindAffinity 解除域名绑定（绑定节点失效时）。
func (p *Pool) unbindAffinity(host string) {
	p.affMu.Lock()
	delete(p.affinity, host)
	p.affMu.Unlock()
	p.markStateDirty()
}

// Len 返回节点总数。
func (p *Pool) Len() int { return len(p.nodes) }

// hostOf 从 target(host:port) 提取主机名（用于域名亲和）。
func hostOf(target string) string {
	h, _, err := net.SplitHostPort(target)
	if err != nil {
		return target
	}
	return h
}

// Dial 通过代理出口连到 target（host:port）。
// 选择策略（优先级从高到低）：
//  1. 锁定模式：全局只用锁定节点。
//  2. 域名亲和：若该域名此前绑定过节点，优先用那个节点（保证同站出口稳定 → 登录态）。
//  3. 自适应：当前主节点优先，失败按评分选次优。
func (p *Pool) Dial(target string) (net.Conn, error) {
	n := len(p.nodes)
	if n == 0 {
		return nil, fmt.Errorf("nodepool: empty")
	}
	host := hostOf(target)

	// 1) 域名亲和：该域名已绑定过节点 → 直接用它（同站出口稳定）
	if idx := p.affinityOf(host); idx >= 0 && idx < n && !p.states[idx].isDisabled() {
		if conn, ok := p.tryDial(idx, target); ok {
			atomic.StoreUint32(&p.idx, uint32(idx))
			return conn, nil
		}
		// 绑定节点失效 → 解除绑定，落到自适应重新选
		p.unbindAffinity(host)
	}

	// 2) 自适应模式：优先试当前主节点
	cur := int(atomic.LoadUint32(&p.idx)) % n
	if !p.states[cur].isDisabled() {
		if conn, ok := p.tryDial(cur, target); ok {
			// 把该域名绑定到命中的节点 → 后续同域名请求都走这里
			p.bindAffinity(host, cur)
			return conn, nil
		}
	}

	// 3) 主节点失败/熔断 → 按评分从优到劣尝试其余节点
	p.rngMu.Lock()
	rng := p.rng
	p.rngMu.Unlock()

	order := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if i != cur {
			order = append(order, i)
		}
	}
	// 按 score 升序（每次比较重新取，随机抖动在 score 里）
	sort.SliceStable(order, func(a, b int) bool {
		return p.states[order[a]].score(rng) < p.states[order[b]].score(rng)
	})

	for _, idx := range order {
		if conn, ok := p.tryDial(idx, target); ok {
			// 新命中的节点接管该域名
			p.bindAffinity(host, idx)
			atomic.StoreUint32(&p.idx, uint32(idx)) // 新的主节点
			return conn, nil
		}
	}
	return nil, fmt.Errorf("nodepool: all %d nodes failed for %s", n, target)
}

// tryDial 尝试用第 idx 个节点建立到 target 的隧道。
// 优先复用该节点已有的 mux 连接（零握手开销），否则新建一条。
func (p *Pool) tryDial(idx int, target string) (net.Conn, bool) {
	node := p.nodes[idx]
	c := &outbound.Client{Node: node, SNI: p.sni, Auth: p.auth, UseECH: p.useECH, Insecure: p.insecure}
	start := time.Now()

	if m := p.takeMux(idx); m != nil {
		conn, err := m.Open(target)
		if err == nil {
			// 必须归还！takeMux 已把它移出池子，若不还回，池子永远是空的，
			// 每个请求都会重新做一次完整的 TLS+WS 握手（实测拖慢到数秒）。
			p.putMux(idx, m)
			p.states[idx].recordSuccess(time.Since(start))
			return conn, true
		}
		m.Close()
	}
	m, err := outbound.DialMux(c)
	if err != nil {
		p.states[idx].recordFailure()
		return nil, false
	}
	conn, err := m.Open(target)
	if err != nil {
		m.Close()
		p.states[idx].recordFailure()
		return nil, false
	}
	p.putMux(idx, m)
	p.states[idx].recordSuccess(time.Since(start))
	return conn, true
}

// SortByLatency 并发探测每个节点的握手延迟，并按延迟升序重排（成功的在前）。
// 这是一次性成本；运行期真正的选择由自适应 score 决定。仅诊断命令使用。
func (p *Pool) SortByLatency() {
	type res struct {
		idx int
		d   time.Duration
		ok  bool
	}
	out := make([]res, len(p.nodes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12)
	for i := range p.nodes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			node := p.nodes[i]
			start := time.Now()
			c := &outbound.Client{Node: node, SNI: p.sni, Auth: p.auth, UseECH: p.useECH, Insecure: p.insecure}
			conn, err := c.DialWS()
			d := time.Since(start)
			ok := err == nil
			if ok {
				conn.Close()
			}
			out[i] = res{idx: i, d: d, ok: ok}
		}(i)
	}
	wg.Wait()
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].ok != out[b].ok {
			return out[a].ok
		}
		return out[a].d < out[b].d
	})
	sorted := make([]entry.Node, len(p.nodes))
	sortedStates := make([]*nodeState, len(p.nodes))
	for i, r := range out {
		sorted[i] = p.nodes[r.idx]
		sortedStates[i] = p.states[r.idx]
		// 用探测延迟初始化 EWMA，后续运行期再自适应修正
		if r.ok {
			sortedStates[i].mu.Lock()
			sortedStates[i].latency = r.d
			sortedStates[i].success = 1
			sortedStates[i].mu.Unlock()
		}
	}
	p.nodes = sorted
	p.states = sortedStates
	// 索引已重排 → 之前的域名亲和绑定全部失效，清空重建（运行期会自然重建）。
	p.affMu.Lock()
	p.affinity = make(map[string]Exit)
	p.affMu.Unlock()
	// 主节点指向探测最快且成功的
	atomic.StoreUint32(&p.idx, 0)
}

// Nodes 返回当前节点顺序（可能是延迟排序后的）。
func (p *Pool) Nodes() []entry.Node { return p.nodes }

// Stats 返回每个节点的成功/失败计数（诊断用）。
func (p *Pool) Stats() []string {
	out := make([]string, 0, len(p.nodes))
	for i, s := range p.states {
		s.mu.Lock()
		out = append(out, fmt.Sprintf("[%d] %s ok=%d fail=%d cons=%d lat=%v dis=%v",
			i, p.nodes[i].Name, s.success, s.fail, s.consecutive, s.latency.Round(time.Millisecond), s.disabled))
		s.mu.Unlock()
	}
	return out
}

// ---- 学到的状态落盘 ----

// savedExit 是落盘形式的出口绑定：节点用地址而不是索引 —— 候选列表在两次
// 运行之间会变（社区源更新），索引不可靠，地址才是稳定标识。
type savedExit struct {
	Direct bool   `json:"direct"`
	Addr   string `json:"addr,omitempty"` // addr:port
}

type savedState struct {
	Affinity      map[string]savedExit `json:"affinity"`
	DirectBlocked map[string]int64     `json:"directBlocked"` // unix ms
}

// loadState 启动时读回上次学到的绑定。地址对不上当前候选集的绑定直接丢弃
// （那个入口已经不在候选里了，保留只会产生一条永远失败的路径）。
func (p *Pool) loadState() {
	if p.stateKey == "" {
		return
	}
	var st savedState
	if _, ok := cache.Load("state", p.stateKey, &st); !ok {
		return
	}
	p.affMu.Lock()
	defer p.affMu.Unlock()
	for host, se := range st.Affinity {
		if se.Direct {
			p.affinity[host] = Exit{Direct: true}
			continue
		}
		for i, n := range p.nodes {
			if net.JoinHostPort(n.Addr, strconv.Itoa(int(n.Port))) == se.Addr {
				p.affinity[host] = Exit{Idx: i}
				break
			}
		}
	}
	now := time.Now()
	for host, ms := range st.DirectBlocked {
		at := time.UnixMilli(ms)
		if now.Sub(at) <= directBlockedTTL {
			p.directBlocked[host] = at
		}
	}
}

// markStateDirty 防抖调度一次落盘：状态变更很频繁（每个新域名一次），
// 但 3 秒内的变更合并成一次写完全够用。
func (p *Pool) markStateDirty() {
	if p.stateKey == "" {
		return
	}
	p.saveMu.Lock()
	if p.savePending {
		p.saveMu.Unlock()
		return
	}
	p.savePending = true
	p.saveMu.Unlock()
	time.AfterFunc(3*time.Second, p.flushState)
}

func (p *Pool) flushState() {
	p.saveMu.Lock()
	p.savePending = false
	p.saveMu.Unlock()
	if p.stateKey == "" {
		return
	}
	st := savedState{
		Affinity:      make(map[string]savedExit, len(p.affinity)),
		DirectBlocked: make(map[string]int64),
	}
	p.affMu.RLock()
	for host, e := range p.affinity {
		if e.Direct {
			st.Affinity[host] = savedExit{Direct: true}
			continue
		}
		if e.Idx >= 0 && e.Idx < len(p.nodes) {
			n := p.nodes[e.Idx]
			st.Affinity[host] = savedExit{Addr: net.JoinHostPort(n.Addr, strconv.Itoa(int(n.Port)))}
		}
	}
	now := time.Now()
	for host, at := range p.directBlocked {
		if now.Sub(at) <= directBlockedTTL {
			st.DirectBlocked[host] = at.UnixMilli()
		}
	}
	p.affMu.RUnlock()
	_ = cache.Save("state", p.stateKey, st)
}

// Verify 做一次真实的传输层建连（TLS+WS+auth），供 serve 启动后的首次连通验证。
// 从当前最优节点开始，最多试 3 个。返回实际走通的节点描述。
func (p *Pool) Verify() (string, error) {
	n := len(p.nodes)
	if n == 0 {
		return "", fmt.Errorf("nodepool: empty")
	}
	var lastErr error
	for i := 0; i < 3 && i < n; i++ {
		idx := (int(atomic.LoadUint32(&p.idx)) + i) % n
		node := p.nodes[idx]
		c := &outbound.Client{Node: node, SNI: p.sni, Auth: p.auth, UseECH: p.useECH, Insecure: p.insecure}
		conn, err := c.DialWS()
		if err == nil {
			conn.Close()
			return fmt.Sprintf("[%d] %s", idx, node.Addr), nil
		}
		lastErr = err
	}
	return "", lastErr
}
