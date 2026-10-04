// Package selector 管理出口节点池：每节点一条复用的 MuxConn，按域名决定
// 直连或代理（M1 精简版：geoip 先验 + 直连阻断冷却；PRD §6.3 的规则分流与
// 延迟优选在 M4 由 internal/rules 与本包的完整版替代）。
package selector

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"netmaster/internal/entry"
	"netmaster/internal/outbound"
)

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

	// muxTarget / idleTrimDelay 是并发余量的两个旋钮，取值见 Config 的同名字段。
	// 存进 Pool 而不是继续读包级变量：包级变量没法按实例配置，改一个池会连带
	// 改掉同进程里所有别的池。
	muxTarget     int
	idleTrimDelay time.Duration
	// warmRetryDelay 是本池"一轮补齐失败"后的首次重试等待（之后指数增长到
	// warmRetryMaxDelay）。存进 Pool 的理由与上面两个旋钮同源，但这里还多一条
	// 更硬的：它在 TopUp 起的 goroutine 里被读（delay := p.warmRetryDelay << ...），
	// 而测试要把它调小才跑得完 —— 直接读包级变量就与测试的赋值构成数据竞争，
	// race detector 在 CI 上抓到过（上一轮测试遗留的 goroutine 还在跑，下一轮
	// 测试已经在改那个包级变量）。
	warmRetryDelay time.Duration
	closed         bool // Close 之后为终态：不再拨号、不再补齐

	// warmRetries: 连续几轮补齐失败（指数退避用；成功即清零）。
	//
	// 必须是原子的：它在 TopUp 起的最多 warmSlots 个 goroutine 里被读-改-写
	// （下面那个 delay := p.warmRetryDelay << min(...) 后紧跟 ++ 是一次读改写），
	// 还会被 time.AfterFunc 的重试链继续触发。裸 int 会丢更新，而丢更新的后果
	// 恰好是这个退避唯一要防的东西：计数被压回 0 → 延迟塌回 2 秒地板 → 一个
	// 已经关掉的 Worker 被我们每 2 秒拨满 warmSlots 次（正是本文件下面注释里
	// 记着的那次实测：Close() 之后 500ms 内 69 次拨号尝试）。
	warmRetries atomic.Int32
	// idleHold: 刚因为空闲回收缩过一轮，先别急着补。
	//
	// 为什么需要它：光在 onMuxDead 里对自回收打个标记是不够的 —— 回收的那一刻
	// 可能正好还有一次拨号在飞，它落地之后的 TopUp 会把池子重新填满（实测
	// 池子在 1 和 2 之间来回抖，每一轮都是一次握手加一个 Durable Object）。
	// 有了它，该不该扩容就变成一个状态问题，而不是谁先到谁说了算。
	idleHold bool

	mu            sync.Mutex
	muxes         map[int]*outbound.MuxConn // 节点索引 -> 活跃 mux
	warming       atomic.Int32              // 正在进行的预热拨号数（并行，非互斥）
	rr            atomic.Uint32             // 流分摊用的轮转游标
	directBlocked map[string]time.Time
	// directDown 记住"这个域名的直连在 TCP 层就失败"（连接被拒/超时）。
	//
	// 与 directBlocked 的分工：directBlocked 是 TLS 层被拦的冷却（代理送出过字节
	// 才写，30min）；directDown 是 TCP 层失败的短记忆（5min）——TCP 都连不上，
	// 大概率是目标本身不可达或本机网络对该目标异常，不值得每个请求都重付一次
	// 完整的拨号超时。TTL 短，因为"目标临时不可达"比"被墙"更常恢复。
	directDown map[string]time.Time
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
		directDown:    make(map[string]time.Time),
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
	if p.warmRetryDelay <= 0 {
		p.warmRetryDelay = warmRetryDelay
	}
	p.pending = newDialPending(p)
	return p
}

// Close 停止等待队列并关闭所有传输（进程退出 / 测试收尾）。
//
// 关闭之后这个池就是终态。这一点不是显然的：关掉传输会触发 onDead 回调，而
// onDead 会无条件 TopUp —— 于是"关掉的池"会在两秒内自己拨号填满，而且再也
// 关不掉（实测：live=2 → Close() → live=0 → 2 秒后 live=2）。进程退出时这等于
// 凭空建一条隧道；测试里这等于留一堆后台 goroutine 在往已关的服务器上打。
func (p *Pool) Close() {
	p.pending.stop()
	p.mu.Lock()
	p.closed = true
	muxes := p.muxes
	p.muxes = make(map[int]*outbound.MuxConn)
	p.mu.Unlock()
	for _, m := range muxes {
		m.Close()
	}
}

// isClosed 报告池是否已被 Close。拨号与补齐都要先看它一眼。
func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
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
//
// 池已 Close 时直接返回 false：redial 的副作用（attach → onDead → TopUp）会把
// 一个"已经关掉的池"重新填满，而那次拨号没人会再去关它。
func (p *Pool) redial() bool {
	if p.isClosed() {
		return false
	}
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
	// 冷启动（池里一条可用传输都没有）时**立刻**对冲，不等那 3 秒。
	//
	// 3 秒的对冲延迟是为"首选节点慢半拍"这种日常情形定的：正常情况下已经有别的
	// 传输在跑，慢一点无所谓。但冷启动没有"慢半拍"可言 —— 第一条传输就是用户能不能
	// 上网的分界线，而启动时往往只有服务端域名解析出的两三个候选，坏窗口里首选那个
	// 可能正被阻断（实测挂着 TCP 拖死 TLS）。此时再等 3 秒纯属白等。
	hedgeDelay := redialHedgeDelay
	if p.liveCount() == 0 {
		hedgeDelay = 0
	}
	hedgeTimer := time.NewTimer(hedgeDelay)
	defer hedgeTimer.Stop()
	for {
		select {
		case r := <-ch:
			pending--
			if r.ok {
				// attach 可能是**失败**的（槽位已被占用、或池已关闭）。
				// 把它当成成功会让调用方以为补齐完成、立刻再来一轮，而新的一轮
				// 又会撞上同一个已占用的槽位。
				if !p.attach(r.idx, r.m) {
					break
				}
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
//
// 同一个节点上已经有活着的传输时**不接管**：直接覆盖会把前一条变成没有下文的
// 孤儿 —— 它还开着、已经不在表里，但 onDead 还指着这个池；等它终于死掉时，
// noteFailure 里的 delete(p.muxes, idx) 会把那条健康的替代品从表里踢掉，
// 而替代品本身还开着、也没人管了。两条隧道、两个 Durable Object、零条可用。
//
// 撞上就把新来的这条还回去，让调用方换个节点重拨。
func (p *Pool) attach(idx int, m *outbound.MuxConn) bool {
	m.OnDead(func(planned, selfTrimmed bool) { p.onMuxDead(idx, planned, selfTrimmed) })
	m.OnIdle(func() { p.trimIdle(idx, m) })
	// 一条刚建好、还没接过任何流的隧道同样算空闲：不主动排这个计时器的话，
	// 它永远等不到 onIdle（onIdle 只在最后一条流结束时才触发），于是用来
	// 补齐的备用隧道会永远留在池里，空闲期收缩到一条就成了一句空话。
	if m.LiveCount() == 0 {
		p.trimIdle(idx, m)
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		m.MarkPlanned()
		m.MarkSelfTrimmed()
		m.Close()
		return false
	}
	if prev, ok := p.muxes[idx]; ok && prev != nil && prev != m && prev.Alive() {
		p.mu.Unlock()
		// 同样要标成"我们自己要它死的"：这条刚拨出来就被退回，而它的死亡回调
		// 会触发 TopUp —— TopUp 又会拨一条、又会撞上同一个已占用的槽位……
		// 于是变成一条自己喂自己的拨号循环（实测目标 2 条时池子反复建了又拆）。
		m.MarkPlanned()
		m.MarkSelfTrimmed()
		m.Close()
		return false
	}
	p.muxes[idx] = m
	p.mu.Unlock()
	// 新传输挂上了：立刻叫醒等待队列里的请求。
	//
	// 队列本身是轮询的（退避 1s→2s→…），而 wait 只等 pendingTimeout(10s) ——
	// 于是"隧道其实已经好了、请求还在等下一轮轮询"是纯浪费。挂载是唯一确切的
	// "有传输可用"时刻，在这里叫醒最省。
	p.pending.wake()
	return true
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
		p.idleHold = true
		p.mu.Unlock()
		// 这条是我们自己要它死的。标成 planned，否则每回收一次就算该节点一次失败，
		// 攒够次数后它会被踢出池子 —— 而它只是空闲，从来没坏过。
		m.MarkPlanned()
		m.MarkSelfTrimmed()
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
func (p *Pool) onMuxDead(idx int, planned, selfTrimmed bool) {
	if p.isClosed() {
		return
	}
	if !planned {
		p.noteFailure(idx)
	}
	if false && selfTrimmed {
		// 空闲回收之后**不要**立刻补齐 —— 补齐会把这一收一放整个抵消掉：
		// 关掉一条、再建一条，隧道数没变、额度没省，还多付一次握手和一次
		// DO 创建。真正需要扩容时由请求路径自己说：排不进队列就是需求超了，
		// 那一刻 pending 会调 TopUp（见 pending.kick）。
		return
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

// warmRetryDelay 是一轮补齐失败后的首次重试等待；之后按指数增长到
// warmRetryMaxDelay 为止。
//
// 这里只是**默认值**：真正被读的是 Pool.warmRetryDelay（构造时从它取值），
// 理由见那个字段的注释。
var warmRetryDelay = 2 * time.Second

// warmRetryMaxDelay 是重试等待的上限。
const warmRetryMaxDelay = 60 * time.Second

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
	return p.liveCountLocked()
}

// liveCountLocked 是 liveCount 的加锁版本。
func (p *Pool) liveCountLocked() int {
	n := 0
	for _, m := range p.muxes {
		if m != nil && m.Alive() {
			n++
		}
	}
	return n
}

// holding 读出刚收缩过这个状态。
func (p *Pool) holding() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idleHold
}

// clearIdleHold 在确认真的有流量之后解除收缩状态。
//
// 谁会调用，代表两种真实需求：
//   - Dial 成功开出流：用户在用，池子该把余量补回来；
//   - pending 队列放行：请求排过队，说明现有的隧道扛不住。
//
// 不由时间过去解除 —— 空闲本来就是我们要的状态。
func (p *Pool) clearIdleHold() {
	p.mu.Lock()
	if !p.idleHold {
		p.mu.Unlock()
		return
	}
	p.idleHold = false
	p.mu.Unlock()
	p.TopUp()
}

// dialWithinBudget 是**请求路径**上的拨号：只有在池里还有名额时才真的拨。
//
// 为什么请求路径要单独一个入口（而不是直接 redial）：断线瞬间上层往往同时涌进
// 十几条连接，池是空的，于是每个请求都会各自拨一条 —— 实测 muxTarget=2、
// 八个节点、并发 Dial，结果池里躺着 6 条隧道。这正是 58ffc12 想省掉的那笔额度
// （每条常连隧道都按 DO 时长计费），只不过当时只修了预热那条路。
//
// 名额满了就返回 false，调用方随即把请求排进等待队列 —— 那正是它该去的地方：
// 已经在飞的拨号会把它带起来。
func (p *Pool) dialWithinBudget() bool {
	// "看名额"和"占名额"必须在同一把锁里做完。分两步的话，八个并发的请求会在
	// 任何一条隧道建成之前**各自**看到"还有 2 个名额"，然后一个不落地全拨出去 ——
	// 实测目标 2 条、池里最后躺着 5~6 条。
	p.mu.Lock()
	if p.closed || p.muxTarget-p.liveCountLocked()-int(p.warming.Load()) <= 0 {
		p.mu.Unlock()
		return false
	}
	p.warming.Add(1) // 预留名额
	p.mu.Unlock()

	ok := p.redial()
	p.warming.Add(-1)
	if ok && !p.isClosed() {
		p.TopUp()
	}
	return ok
}

// TopUp 把活跃传输补到 muxTarget 条（后台进行，不阻塞调用方）。每成功一条就
// 再检查一次，直到补满。
func (p *Pool) TopUp() {
	if p.isClosed() {
		return
	}
	if p.holding() {
		return // 刚收缩过；等到真的有需求（clearIdleHold）再补
	}
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
			if p.isClosed() {
				return
			}
			if ok {
				p.warmRetries.Store(0)
				p.TopUp()
				return
			}
			// 这一次没拨出来。补齐失败不能就此搁置（否则启动时正好撞上一次坏节点，
			// 这个池就长期少一条，直到下一个请求碰巧触发补齐），但也不能立刻重试 ——
			// 全灭时那会变成热循环。
			//
			// 退避要**指数增长**：固定 2 秒一轮的话，一个已经关掉的 Worker 会让我们
			// 每 2 秒拨满 warmSlots 次，永远不停（对抗评审实测：Close() 之后 500ms
			// 内又发起了 69 次拨号尝试）。指数退避把它压到一分钟一次，代价可以忽略，
			// 而真恢复过来的那一次最多晚一分钟接上 —— 那种情况下用户早就重开了客户端。
			//
			// 计数与取延迟必须合成一次原子操作：先 Load 再 Store 是读-改-写，
			// 几个 goroutine 同时失败时照样丢更新，退避就被压回地板。
			prev := p.warmRetries.Add(1) - 1
			delay := p.warmRetryDelay << min(prev, 5)
			if delay > warmRetryMaxDelay {
				delay = warmRetryMaxDelay
			}
			time.AfterFunc(delay, p.TopUp)
		}()
	}
}

// Warm 起 muxTarget 条传输（serve 启动后调用，与请求路径解耦）。
func (p *Pool) Warm() { p.TopUp() }

// dialMux 建立节点 idx 的传输（不登记进池）。
func (p *Pool) dialMux(idx int) (*outbound.MuxConn, error) {
	client := &outbound.Client{
		Node:     p.nodeAt(idx),
		SNI:      p.sni,
		Password: p.password,
		UseECH:   p.useECH,
		Insecure: p.insecure,
	}
	return outbound.DialMux(client)
}

// nodeAt 取第 idx 个候选。
//
// 必须持锁：候选表在启动之后仍会增长（见 AddNodes），而 dialMux 跑在拨号
// goroutine 里、与那次 append 并发。
func (p *Pool) nodeAt(idx int) entry.Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nodes[idx]
}

// AddNodes 把新一批候选并进池子（按 addr:port 去重），返回真正新增的条数，
// 并触发一次补齐。
//
// 为什么需要它：入口候选分两批到 —— 服务端域名的 DNS 解析是毫秒级的，社区优选
// 列表要一两秒。启动路径先用前者把隧道立起来（用户不必等社区源），后者到了再并进来，
// 于是"启动那一刻的候选表"不再是终局，池子能越用越宽。
//
// 只 append、不删除、不重排：候选的 index 是 dead/fails 两张表的键，也是
// dialMux 的入参，重排会让"这个 index 是哪个节点"在并发中改变。
func (p *Pool) AddNodes(nodes []entry.Node) int {
	if len(nodes) == 0 {
		return 0
	}
	key := func(n entry.Node) string { return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.Port))) }
	p.mu.Lock()
	seen := make(map[string]bool, len(p.nodes)+len(nodes))
	for _, n := range p.nodes {
		seen[key(n)] = true
	}
	added := 0
	for _, n := range nodes {
		k := key(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		p.nodes = append(p.nodes, n)
		added++
	}
	p.mu.Unlock()
	if added > 0 && !p.isClosed() {
		p.TopUp()
	}
	return added
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

// recheckParallel 是周期探活的并发上限：只探前几个节点，一波就该跑完。
const recheckParallel = 5

// RecheckTop 对当前节点序的前 n 个做一次 TCP+TLS 握手探活（不带 WS 升级 ——
// 升级会真的建 DO，空闲期凭空烧额度）。
//
// 为什么需要它：运行期原本没有周期探活，节点健康完全靠"真实流量撞上失败"来
// 发现。空闲一小时后第一批请求可能撞上整批死节点，体验是"明明刚才还好好的"。
// 成功清零失败计数、失败记一次（连续 nodeFailLimit 次判死，与流量侧同一套账，
// 所以探活自己不会把节点冤死）。
//
// 返回探活的节点里握上手几个（日志用）。调用方给 ctx 设预算：探活是后台的，
// 不许跟真实流量抢时间。
func (p *Pool) RecheckTop(ctx context.Context, n int) int {
	p.mu.Lock()
	if n > len(p.nodes) {
		n = len(p.nodes)
	}
	if n <= 0 {
		p.mu.Unlock()
		return 0
	}
	top := make([]entry.Node, n)
	copy(top, p.nodes[:n])
	sni, insecure, useECH := p.sni, p.insecure, p.useECH
	p.mu.Unlock()

	ech := []byte(nil)
	if useECH {
		ech = echConfigFor(ctx, sni)
	}

	var alive atomic.Int32
	sem := make(chan struct{}, recheckParallel)
	var wg sync.WaitGroup
	for i := range top {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, conn := probeOne(ctx, top[i], sni, insecure, ech)
			if conn != nil {
				_ = conn.Close() //nolint:errcheck
				p.noteSuccess(i)
				alive.Add(1)
				return
			}
			p.noteFailure(i)
		}(i)
	}
	wg.Wait()
	return int(alive.Load())
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.nodes)
}

// Alive 返回**当前还没被判死**的节点数。
//
// 为什么不能拿 Len() 代替：Len() 数的是池子里有多少节点，不是有多少能用。
// 实测过一个池里 49 个节点全死的情形，于是 `Len() != 0`，调用方据此认为
// "还有出口可用"，既不去直连兜底也不报错 —— 用户看到的是客户端打印 ready、
// 网页却一张都打不开，而失败原因不在任何用户能看到的地方。
//
// 语义与 pickNode 对齐：**全判死时返回 0**，不是返回 len(nodes)。
// pickNode 在全死时会复活全部节点继续赌一把，但"曾经全部失败过"本身就是
// 直连兜底比再等一轮退避重连更划算的信号。
func (p *Pool) Alive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.nodes {
		if !p.dead[i] {
			n++
		}
	}
	return n
}

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
	if p.Len() == 0 {
		return nil, errNoUsableExit
	}
	for attempt := 0; attempt < 2; attempt++ {
		idx, m := p.pickMux()
		if m != nil {
			conn, err := m.Open(target)
			if err == nil {
				p.noteSuccess(idx)
				// 真的开出一条流了：空闲期结束，该把余量补回来。
				p.clearIdleHold()
				return conn, nil
			}
			if m.Alive() {
				// 流被拒（0x01-0x03）是服务端裁决，换节点也是同一裁决。
				p.noteFailure(idx)
				return nil, err
			}
			// 传输在半路死了：记一次失败（到阈值判死）并重建
			p.noteFailure(idx)
			if !p.dialWithinBudget() {
				break
			}
			continue
		}
		// 没有可用传输：当场重建一条（有名额才拨）
		if !p.dialWithinBudget() {
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

// ShouldTryFragDirect 回答"这个域名现在值不值得先试一次分片直连"（实现 proxy.FragTryer）。
//
// 默认是"值得"，这是刻意的：分流判 proxy 只说明这个站通常需要代理，不代表直连一定
// 不通 —— 而**用户装这个软件本身就说明普通直连不通**，所以这里不问"明文行不行"，
// 直接按分片试（调用方会带着分片起步，见 proxy.relayWithReplay 的 frag）。分片穿过去
// 就是 2 跳，绕过去是 3 跳，差的是整个 Worker 出站那一段。
//
// 记忆决定要不要再试：
//   - fragDirect 新鲜（6h）→ 值得，且下次直接带分片起步；
//   - directBlocked 新鲜（30min，隧道真的送出过字节时写）→ 不值得，直接走隧道；
//   - 没有记忆 → 值得试一次（试错的成本有上界，见 proxy.relayFragProbeWait）。
func (p *Pool) ShouldTryFragDirect(host string) bool {
	host = hostOf(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fragDirectFreshLocked(host) {
		return true
	}
	if at, has := p.directBlocked[host]; has {
		if time.Since(at) > directBlockedTTL {
			delete(p.directBlocked, host)
		} else {
			return false
		}
	}
	return true
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
// 而 shouldDirect 这一侧读写的是不带端口的键。不归一的话，
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

// ForgetFragDirect 忘掉"这个域名要分片直连"（实现 proxy.FragDirecter）。
//
// 为什么需要它：fragDirect 的优先级**高于** directBlocked（见字段注释），所以一条
// 过期的分片记忆会连带压掉"这个域名该走代理"这条结论。如果链路一变（换了网络、
// 换了中间设备），分片开始失效而记忆还有 6 小时，每条连接都会白付约 400ms 的
// 分片延迟再落代理。分片实测失败时必须把它摘掉，否则这个代价会持续很久。
func (p *Pool) ForgetFragDirect(host string) {
	host = hostOf(host)
	p.mu.Lock()
	delete(p.fragDirect, host)
	p.mu.Unlock()
}

// RetryProxy 在直连被判定阻断后改用代理，并记下"这个域名别再走直连"。
func (p *Pool) RetryProxy(target string) (net.Conn, error) {
	return p.Dial(target)
}

// directDownTTL 是"直连 TCP 层失败"这条短记忆的保留时长。比 directBlockedTTL
// 短得多：目标临时不可达（站点重启、本机网络抖动）远比被墙恢复得快，记长了会把
// 已经恢复的域名按在代理上多绕 5 分钟。
const directDownTTL = 5 * time.Minute

// NoteDirectDown 记下"该域名的直连在 TCP 层失败"。由 proxy 层在规则直连拨号
// 失败时调用；调用方通常紧接着会试代理兜底，这条记忆只是让 5 分钟内的后续请求
// 别再重复付一次完整的拨号超时。
func (p *Pool) NoteDirectDown(host string) {
	host = hostOf(host) // 入参可能带端口，键必须归一（同 NoteFragDirect）
	p.mu.Lock()
	p.directDown[host] = time.Now()
	p.mu.Unlock()
}

// DirectDownRecently 报告该域名近期是否发生过直连 TCP 层失败（5min 内）。
func (p *Pool) DirectDownRecently(host string) bool {
	host = hostOf(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	at, has := p.directDown[host]
	if !has {
		return false
	}
	if time.Since(at) > directDownTTL {
		delete(p.directDown, host)
		return false
	}
	return true
}

// SweepExpired 清掉三张记忆表里已过期的条目。
//
// 过期删除原本是**惰性**的（读到才删）：三张表的读取口都顺手删过期键，但"没再
// 被访问"的过期条目永远躺在 map 里 —— 桌面进程一开几天，浏览过的每个域名都占
// 一条，那是无界增长。挂进周期探活的节拍里每 10 分钟扫一次，增长就有界了。
func (p *Pool) SweepExpired() {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for h, at := range p.directBlocked {
		if now.Sub(at) > directBlockedTTL {
			delete(p.directBlocked, h)
		}
	}
	for h, at := range p.directDown {
		if now.Sub(at) > directDownTTL {
			delete(p.directDown, h)
		}
	}
	for h, at := range p.fragDirect {
		if now.Sub(at) > fragDirectTTL {
			delete(p.fragDirect, h)
		}
	}
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

// VerifyOnLive 用**已经建好的**传输做端到端验证：传输层（TLS+WS+认证）由预热完成，
// 这里验的是"出口真的能把流送出去"（M0 E6/E7：443 + 非 CF 目标才是稳定组合）。
//
// 与 Verify 的区别是**它自己不拨号**。Verify 会串行试最多 3 个节点，而坏窗口里一个
// 挂着的 IP 能耗满拨号超时（实测 6s 级）—— 于是"预热"要等它试完才开始，而预热才是
// 让用户请求能用上的那件事（最坏实测 18s 白白串在关键路径上）。这里改成等预热的
// 结果：谁先建好就用谁，等待有上限。
func (p *Pool) VerifyOnLive(target string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		if idx, m := p.liveMux(); m != nil {
			conn, err := m.Open(target)
			if err == nil {
				_ = conn.Close()
				return p.nodeAt(idx).Addr, nil
			}
			// 流被拒（0x01-0x03）是服务端裁决，换传输也是同一裁决，直接报出来。
			if m.Alive() {
				p.noteFailure(idx)
				return "", err
			}
			p.noteFailure(idx)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("selector: no live transport within %s", wait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Nodes 返回节点列表的**快照**（诊断用）。
//
// 快照而不是内部切片：候选表会被 AddNodes 追加，把内部切片交出去就等于让调用方
// 在无锁的情况下与那次 append 赛跑。
func (p *Pool) Nodes() []entry.Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]entry.Node, len(p.nodes))
	copy(out, p.nodes)
	return out
}
