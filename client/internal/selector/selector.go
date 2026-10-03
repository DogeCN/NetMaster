// Package selector 管理出口节点池：每节点一条复用的 MuxConn，按域名决定
// 直连或代理（M1 精简版：geoip 先验 + 直连阻断冷却；PRD §6.3 的规则分流与
// 延迟优选在 M4 由 internal/rules 与本包的完整版替代）。
package selector

import (
	"fmt"
	"math/rand"
	"net"
	"sort"
	"sync"
	"sync/atomic"
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
	// MuxTarget 同时保持的传输条数。0 = DefaultMuxTarget。
	// 面向用户的入口是 config.json 的 "tunnels"（见 cmd/netmaster）。
	MuxTarget int
	// IdleTrimDelay 空闲多久之后回收多余隧道。0 = 包内默认值。
	// 只在测试里注入：它是内部的额度保护参数，不该出现在用户的配置文件里
	// （用户的正常心智里只有"几条隧道"，没有"空闲多久收"）。
	IdleTrimDelay time.Duration
}

// directBlockedTTL 是"该域名禁用直连"的冷却时长。
//
// 冷却而非永久：一次网络抖动不该让这个域名从此失去直连（国内站会因此绕远路）。
// 注意现在只有"代理真的送出了字节"才会写这条记忆（见 NoteProxyConfirmed），
// 所以它已经是一个强信号了，但长度仍按"网络会变"来给。
const directBlockedTTL = 30 * time.Minute

type Pool struct {
	nodes    []entry.Node
	sni      string
	password string
	useECH   bool
	insecure bool

	dialTimeout time.Duration // 直连拨号超时；0 = 默认 15s
	geo         GeoResolver

	// muxTarget / idleTrimDelay 是并发余量的两个旋钮，取值见 Config 的同名字段。
	// 存进 Pool 而不是继续读包级变量：包级变量没法按实例配置，改一个池会连带
	// 改掉同进程里所有别的池。
	muxTarget     int
	idleTrimDelay time.Duration

	mu            sync.Mutex
	muxes         map[int]*outbound.MuxConn // 节点索引 -> 活跃 mux
	warming       atomic.Int32              // 正在进行的预热拨号数（并行，非互斥）
	rr            atomic.Uint32             // 流分摊用的轮转游标
	directBlocked map[string]time.Time
	direct        map[string]bool // host -> 已学习的直连/代理粘性
	// fragDirect 记住"这个域名只有把 TLS 首段分片才能直连"（TLS-RF，见 proxy/tlsfrag.go）。
	// 存在这里的含义是"分片直连已验证可行"，因此它的优先级高于 directBlocked：
	// 后者只是"明文 SNI 挨过一次拦"的冷却，不该把学到的能力作废。
	fragDirect map[string]time.Time
	fails      map[int]int  // 节点索引 -> 连续失败次数
	dead       map[int]bool // 连续失败到阈值判死，等待重随机时复活
	pending    *dialPending // 断线等待队列与退避重连
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
		fragDirect:    make(map[string]time.Time),
		fails:         make(map[int]int),
		dead:          make(map[int]bool),
		muxTarget:     cfg.MuxTarget,
		idleTrimDelay: cfg.IdleTrimDelay,
	}
	if p.muxTarget <= 0 {
		p.muxTarget = DefaultMuxTarget
	}
	if p.idleTrimDelay <= 0 {
		p.idleTrimDelay = idleTrimDelay
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

// redialHedgeDelay 是对冲拨号的启动延迟：首选节点这么久还没建好传输，就同时
// 拨第二个节点。实测大陆网络上部分 CF IP 会挂着 TCP 但拖死 TLS/WS（15s 才超时），
// 单节点串行拨号会让浏览器整页加载吃满超时——对冲把最坏情况从"一次坏运气 15s"
// 压到"3s + 第二个节点的建连时间"。
const redialHedgeDelay = 3 * time.Second

// redial 重建一条传输；成功返回 true（失败时 mux 不入表）。
// 首选节点慢半拍时自动对冲第二个节点，先建好的赢。
func (p *Pool) redial() bool {
	first := p.pickNode()
	if first < 0 {
		return false
	}
	type result struct {
		idx int
		m   *outbound.MuxConn
		ok  bool
	}
	ch := make(chan result, 2)
	dial := func(idx int) {
		m, err := p.dialMux(idx)
		if err != nil {
			p.noteFailure(idx)
			ch <- result{idx: idx}
			return
		}
		ch <- result{idx: idx, m: m, ok: true}
	}
	go dial(first)

	pending := 1
	hedged := false
	hedgeTimer := time.NewTimer(redialHedgeDelay)
	defer hedgeTimer.Stop()
	for {
		select {
		case r := <-ch:
			pending--
			if r.ok {
				p.attach(r.idx, r.m)
				// 败者（若还在拨）晚些完成时会留下一条空闲传输：回收掉
				if left := pending; left > 0 {
					go func() {
						for ; left > 0; left-- {
							if r := <-ch; r.ok {
								r.m.Close()
							}
						}
					}()
				}
				return true
			}
			if pending == 0 {
				return false
			}
		case <-hedgeTimer.C:
			if hedged {
				continue // 已对冲：无限等结论（dial 自带超时，goroutine 必然收尾）
			}
			hedged = true
			if second := p.pickNodeExcluding(first); second >= 0 {
				pending++
				go dial(second)
			}
		}
	}
}

// idleTrimDelay 是"空闲多久之后把这条多余隧道收掉"的等待。
//
// 为什么需要：服务端每条连接都有一个 185 秒的判死定时器，而它是 pending timer
// （会阻止 DO 休眠，m0 E4/E9），空闲期按 DO 时长计费（免费版有额度，但不是无限的）。
// 并发余量只在**有流量**时才有价值：突发期保持 muxTarget 条，空闲期收缩到 1 条。
// 变量而非常量：测试要把它降到毫秒级来断言回收行为（见 trim_idle_test.go）；
// Config.IdleTrimDelay 优先于它（per-pool 覆盖，New 在构造时定下）。
var idleTrimDelay = 45 * time.Second

// attach 把建好的传输登记进池并挂上回调。
func (p *Pool) attach(idx int, m *outbound.MuxConn) {
	m.OnDead(func(planned bool) { p.onMuxDead(idx, planned) })
	m.OnIdle(func() { p.trimIdle(idx, m) })
	p.mu.Lock()
	p.muxes[idx] = m
	p.mu.Unlock()
}

// trimIdle 回收空闲的多余隧道：等一会儿仍然没有流、且池里还有别的可用隧道，就关掉它。
// 只剩最后一条时留着——否则客户端会陷入"必须重连才能上网"的状态。
func (p *Pool) trimIdle(idx int, m *outbound.MuxConn) {
	time.AfterFunc(p.idleTrimDelay, func() {
		if m.LiveCount() > 0 {
			return // 又来流量了
		}
		p.mu.Lock()
		cur, ok := p.muxes[idx]
		if !ok || cur != m {
			p.mu.Unlock()
			return // 已经换掉了
		}
		others := 0
		for i, mm := range p.muxes {
			if i != idx && mm != nil && mm.Alive() {
				others++
			}
		}
		if others == 0 {
			p.mu.Unlock()
			return // 最后一条，留着
		}
		delete(p.muxes, idx)
		p.mu.Unlock()
		m.Close() // 死亡回调会触发补齐
	})
}

// onMuxDead 传输终止时的处理。
//
// planned（服务端预算回收）不算节点故障——那是设计好的续命点（m0-findings E8：
// 每条连接一生约 30 次出站建连），把它记成失败会把好节点误判死。
//
// 两种情况都要在后台把下一条连接备好：资源密集型页面（Netflix 之类一次开
// 几十条流）会在回收瞬间堆出一大批请求，让它们全挤在"下一次拨号"的关键
// 路径上，实测就是整页加载超时。
func (p *Pool) onMuxDead(idx int, planned bool) {
	if !planned {
		p.noteFailure(idx)
	}
	// 不管还剩几条活着都补到 muxTarget：资源密集页面靠的就是这份余量。
	p.TopUp()
}

// muxTarget 是同时保持的传输（WebSocket）条数。
//
// 为什么必须 >1：服务端每条连接一生只有约 30 次出站建连的子请求预算
// （m0-findings E8），到点整条连接被回收。而资源密集页面（Netflix、Instagram
// 首屏）一次开 50+ 条流，单条连接必然在加载中途被回收，在途流全死、浏览器重试，
// 表现为"整页加载超时"。4 条连接 = 4 份预算 ≈ 120 条流的中位余量。
//
// 分摊到多条连接不破坏出口 IP 稳定性：出口由服务端的 Router DO 映射决定
// （v0.2.1 修复后跨会话生效），入口连接换了，映射照旧命中同一条中继。
//
// 导出成 DefaultMuxTarget：config.json 的 "tunnels" 缺省时用它，命令行帮助里
// 也要把这个数印出来，用户才知道自己填的 2 是"少一半"而不是"随便一个数"。
const DefaultMuxTarget = 4

// MaxMuxTarget 是接受的上限。免费版 DO 有 13,000 GB-s/日的时长额度，而每条常连
// 隧道都会按 DO 时长计费（空且能休眠的不计），所以条数不是"越多越稳"而是
// 一笔要算的账。超上限直接拒绝启动，好过静默开一堆隧道把额度花光。
const MaxMuxTarget = 8

// warmSlots 是同时进行的预热拨号上限：全并发会和请求路径抢节点池。
const warmSlots = 3

// warmRetryDelay 是一轮补齐失败后的重试等待。
var warmRetryDelay = 2 * time.Second

// pickMux 在活跃传输里轮转选一条：并发流分摊到多条连接，而不是把整份预算压在
// 一条上。选中的那条刚好死掉就换下一条。
func (p *Pool) pickMux() (int, *outbound.MuxConn) {
	for try := 0; try < 2; try++ {
		p.mu.Lock()
		idxs := make([]int, 0, len(p.muxes))
		for i, m := range p.muxes {
			if m != nil && m.Alive() {
				idxs = append(idxs, i)
			}
		}
		sort.Ints(idxs) // 稳定顺序，轮转才有意义
		var (
			m   *outbound.MuxConn
			idx int
		)
		if len(idxs) > 0 {
			idx = idxs[int(p.rr.Add(1)-1)%len(idxs)]
			m = p.muxes[idx]
		}
		p.mu.Unlock()
		if m == nil {
			return -1, nil
		}
		if m.Alive() {
			return idx, m
		}
	}
	return -1, nil
}

// liveCount 返回活跃传输条数（补齐/诊断用）。
func (p *Pool) liveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.muxes {
		if m != nil && m.Alive() {
			n++
		}
	}
	return n
}

// TopUp 把活跃传输补到 muxTarget 条（后台进行，不阻塞调用方）。每成功一条就
// 再检查一次，直到补满。
func (p *Pool) TopUp() {
	// 在途的拨号也要算进"已经占用的名额"：每个 warming 槽位最多会再添一条隧道。
	// 不减掉它就会出现超发 —— 两条并行的 TopUp 各看到 missing=2，就真的拨出 4 条
	// （本机实测能稳定复现到多出 1 条）。多出来的隧道不是白搭：空闲回收要等
	// idleTrimDelay，而在此期间它按 DO 时长计费，正是免费版最紧张的额度。
	missing := p.muxTarget - p.liveCount() - int(p.warming.Load())
	if missing <= 0 {
		return
	}
	// 并行补齐，最多 warmSlots 个。串行补齐会让一次突发等 3× 拨号时间（实测每条
	// 隧道 1~2 秒），而"突发后的第一波请求"恰恰是最在意延迟的时候——那几秒正好
	// 压在用户首屏上。
	for i := 0; i < missing; i++ {
		if p.warming.Add(1) > warmSlots {
			p.warming.Add(-1)
			return
		}
		go func() {
			ok := p.redial()
			// 先释放名额，再决定要不要继续补。顺序反了会**永远少一条**：goroutine 里
			// 的 TopUp 看到的 warming 至少包含自己，于是它永远认为还有一条在路上，
			// 于是永远不再补 —— 本机实测池会停在 muxTarget-1 上直到下次拨号事件。
			p.warming.Add(-1)
			if ok {
				p.TopUp()
				return
			}
			// 这一次没拨出来。补齐失败不能就此搁置（否则启动时正好撞上一次坏节点，
			// 这个池就长期少一条，直到下一个请求碰巧触发补齐），但也不能立刻重试 ——
			// 全灭时那会变成热循环。退避后重来一轮。
			time.AfterFunc(warmRetryDelay, p.TopUp)
		}()
	}
}

// Warm 起 muxTarget 条传输（serve 启动后调用，与请求路径解耦）。
func (p *Pool) Warm() { p.TopUp() }

// dialMux 建立节点 idx 的传输（不登记进池）。
func (p *Pool) dialMux(idx int) (*outbound.MuxConn, error) {
	client := &outbound.Client{
		Node:     p.nodes[idx],
		SNI:      p.sni,
		Password: p.password,
		UseECH:   p.useECH,
		Insecure: p.insecure,
	}
	return outbound.DialMux(client)
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

// pickNodeExcluding 同 pickNode，但避开 exclude（对冲拨号的第二个节点用）。
func (p *Pool) pickNodeExcluding(exclude int) int {
	p.mu.Lock()
	live := make([]int, 0, len(p.nodes))
	for i := range p.nodes {
		if i != exclude && !p.dead[i] {
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

// MuxTarget 返回这个池实际生效的隧道条数（诊断/启动日志用）。
func (p *Pool) MuxTarget() int { return p.muxTarget }

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
		idx, m := p.pickMux()
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
	m, err := p.dialMux(idx)
	if err != nil {
		return nil, err
	}
	p.attach(idx, m)
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
	// 分片记忆优先于"别直连"的冷却。这个域名我们已经验证过"分片直连走得通"，
	// 而冷却期的由来只是"明文 SNI 挨过一次拦"——按冷却把它丢给代理，等于把
	// 已经付过代价学到的能力作废，逼着每次连接重走一遍失败的明文探测。
	if p.fragDirectFreshLocked(host) {
		p.mu.Unlock()
		return true
	}
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

// fragDirectTTL 是"这个域名需要分片直连"这条记忆的保留时长。
//
// 比 directBlockedTTL 长得多：directBlocked 是一次失败就能下的结论（冷却短一点，
// 代价是偶尔多绕一跳）；而分片记忆是"试了才知道"的结论 —— 重新学一遍要额外付一次
// 明文首段被拦的探测（3 秒窗口）+ 一次分片重试。留短了等于反复交学费。
const fragDirectTTL = 6 * time.Hour

// fragDirectFreshLocked 查分片记忆是否仍在保留期内。调用方必须已持 p.mu。
func (p *Pool) fragDirectFreshLocked(host string) bool {
	at, has := p.fragDirect[host]
	if !has {
		return false
	}
	if time.Since(at) > fragDirectTTL {
		delete(p.fragDirect, host)
		return false
	}
	return true
}

// NoteFragDirect 记下"该域名的直连首段要分片发"（实现 proxy.FragDirecter）。
//
// 由 proxy 层在"明文首段被拦、但分片重试成功"时调用。顺带清掉该域名的直连冷却：
// 既然分片这条路已验证可行，再按冷却强制走代理就是白白多付一跳。
//
// ⚠️ 入参统一过 hostOf：proxy 侧拿到的是 CONNECT 目标，形如 "example.com:443"；
// 而 shouldDirect / DialAuto 这一侧读写的是不带端口的键。不归一的话，
// 写进去的条目永远读不回来 —— 记忆与冷却都会静默失效（评审 R-KEY 抓到的就是这条）。
func (p *Pool) NoteFragDirect(host string) {
	host = hostOf(host)
	p.mu.Lock()
	p.fragDirect[host] = time.Now()
	delete(p.directBlocked, host)
	p.mu.Unlock()
}

// NeedsFragDirect 查该域名是否已记住需要分片直连（实现 proxy.FragDirecter）。
func (p *Pool) NeedsFragDirect(host string) bool {
	host = hostOf(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fragDirectFreshLocked(host)
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
	delete(p.direct, host)
	p.mu.Unlock()
	return p.Dial(target)
}

// NoteProxyConfirmed 在代理隧道真的送出第一个字节之后调用（实现 proxy.ProxyConfirmer）。
//
// 为什么不在 RetryProxy 里就记 blocked：RetryProxy 只表示"我们开始试代理了"，
// 那次尝试本身可能因为节点故障而失败，与"这个域名被墙"毫无关系。把两者记成同一件事，
// 会让一次代理侧的偶发失败把一个本来能直连的域名按进强制代理。
//
// 时长比本文件其它记忆都长：只有真被墙、且分片也没救回来的域名才会走到这里，
// 所以"落到代理"本身就是一个强信号。
func (p *Pool) NoteProxyConfirmed(host string) {
	host = hostOf(host) // 同 NoteFragDirect：入参可能带端口，键必须归一
	p.mu.Lock()
	p.directBlocked[host] = time.Now()
	p.mu.Unlock()
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
	//
	// 依次试前 3 个节点（按探测延迟排序）：单节点抽样会把"恰好分到一个坏 IP"
	// 报成 tunnel failed，而浏览实际是好的——误报比不报更吓人。
	var lastErr error
	for i := 0; i < 3 && i < len(p.nodes); i++ {
		if _, err := p.dialNode(i, "www.google.com:443"); err != nil {
			lastErr = err
			continue
		}
		return p.nodes[i].Addr, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("selector: no nodes tried")
	}
	return "", lastErr
}

// Nodes 返回节点列表（诊断用）。
func (p *Pool) Nodes() []entry.Node { return p.nodes }
