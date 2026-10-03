package selector

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"netmaster/internal/entry"
	"netmaster/internal/outbound"
)

// TestMuxTargetDefault 缺省时用包内默认值。没写这一条，配置面一改就会
// 悄悄改变所有没配 tunnels 的用户的并发度。
func TestMuxTargetDefault(t *testing.T) {
	p := New(Config{Nodes: []entry.Node{{Addr: "127.0.0.1", Port: 443}}})
	if p.MuxTarget() != DefaultMuxTarget {
		t.Fatalf("default mux target = %d, want %d", p.MuxTarget(), DefaultMuxTarget)
	}
	if p.idleTrimDelay != idleTrimDelay {
		t.Fatalf("default idle trim = %v, want %v", p.idleTrimDelay, idleTrimDelay)
	}
}

// TestMuxTargetFromConfig 是 config.json "tunnels" 的端到端门：真的起一个 TLS
// 对端、真的拨出去，看活跃隧道停在配置写的那个数上。
//
// 为什么不用"断言字段被赋值"了事：那样的话 TopUp 忘了读这个字段照样全绿 ——
// 上一轮"文档写了 idle 回收、代码没接线"就是这个形状的错误（A8 F1）。
// 这里断的是"配置值 → 实际并发条数"这条真实链路。
func TestMuxTargetFromConfig(t *testing.T) {
	srv := miniWSTLS(t)
	defer srv.Close()
	host, port := srvAddr(t, srv)

	p := New(Config{
		// 4 个节点条目都指向同一个对端：给 pickNode 足够的落点，
		// 让 TopUp 停下来的原因只能是"够了"，不会是"没节点可用"。
		Nodes:     repeatNodes(host, port, 4),
		SNI:       "example.com",
		Password:  "pw",
		Insecure:  true, // httptest 用的是自签证书
		MuxTarget: 2,
	})
	defer p.Close()

	p.Warm()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && p.liveCount() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := p.liveCount(); n != 2 {
		t.Fatalf("live tunnels = %d, want exactly 2 (config MuxTarget is not driving TopUp)", n)
	}
	// 稳定期：TopUp 是递归补齐的，多拨一条不会自己收回去。多等一会儿，
	// 抓的是"它还在继续补"这种只在递归里出现的超发。
	time.Sleep(500 * time.Millisecond)
	if n := p.liveCount(); n != 2 {
		t.Fatalf("live tunnels = %d after settling, want 2 (TopUp keeps topping past the target)", n)
	}
}

// TestIdleTrimDelayFromConfig 反向门：Config 里的值必须压过包级变量。
//
// 把包级变量设成 1 小时、配置里写 20ms，然后断言多余隧道确实被回收了。
// 去掉 Config 覆盖（回到读包级变量）这条会失败 —— 回收永远不会发生。
func TestIdleTrimDelayFromConfig(t *testing.T) {
	srv, wsURL := miniWS(t)
	defer srv.Close()

	old := idleTrimDelay
	idleTrimDelay = time.Hour // 包级默认故意设成"永远不回收"
	defer func() { idleTrimDelay = old }()

	m0, err := outbound.DialMuxPlain(wsURL, "pw")
	if err != nil {
		t.Fatalf("dial mux 0: %v", err)
	}
	m1, err := outbound.DialMuxPlain(wsURL, "pw")
	if err != nil {
		t.Fatalf("dial mux 1: %v", err)
	}
	defer m0.Close()
	defer m1.Close()

	p := New(Config{
		Nodes:         repeatNodes("127.0.0.1", 443, 2),
		SNI:           "example.com",
		IdleTrimDelay: 20 * time.Millisecond,
	})
	p.attach(0, m0)
	p.attach(1, m1)

	// m0 上留一条**开着**的流：它不能被回收，于是唯一可能被回收的就是 m1。
	// （两条都空着的话，谁先到计时器谁走，这条用例就变成掷硬币 —— attach 现在
	// 会为没有流的隧道也排计时器，正是为了让"备用隧道"也能收缩。）
	hold, err := m0.Open("example.com:443")
	if err != nil {
		t.Fatalf("open stream on mux 0: %v", err)
	}
	defer hold.Close()

	st, err := m1.Open("example.com:443")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = st.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, stillThere := p.muxes[1]
		p.mu.Unlock()
		if !stillThere {
			return // 回收成功 —— 只能来自 Config.IdleTrimDelay
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("idle mux survived: Config.IdleTrimDelay is not overriding the package default")
}

// miniWSTLS 是 muxHandler 的 TLS 版本：出站真实路径走的是 wss（TLS + WS），
// 明文对端只能用来造现成的 MuxConn，验不了"配置 → 拨号"这一段。
func miniWSTLS(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(muxHandler())
}

func repeatNodes(host string, port int, n int) []entry.Node {
	nodes := make([]entry.Node, n)
	for i := range nodes {
		nodes[i] = entry.Node{Addr: host, Port: uint16(port)}
	}
	return nodes
}

func srvAddr(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	return u.Hostname(), port
}

// TestWarmupRetriesAfterFailedDial 钉住"一轮补齐失败要退避重来"。
//
// 背景：补齐失败时原实现直接返回，既不重试也不记账。后果不是慢，是**长期少一条** ——
// 启动时恰好撞上一个坏节点，这个池就一直是 muxTarget-1，直到下一个请求碰巧触发
// 补齐为止。用户的体感是"有时候打开视频就是卡"。
func TestWarmupRetriesAfterFailedDial(t *testing.T) {
	old := warmRetryDelay
	warmRetryDelay = 20 * time.Millisecond
	defer func() { warmRetryDelay = old }()

	// 前两次升级被拒（TLS 握手照常成功，失败点在 HTTP 层，与"边缘回 403"同形），
	// 第三次放行。
	//
	// 计数用原子量而不是普通 int：处理函数跑在 httptest 自己的 goroutine 上，
	// 而失败时要在测试 goroutine 里读它。用普通 int 的话，这条用例自己就是一个
	// 数据竞争 —— 而 CI 里正好有一道 -race 的门会把它揪出来。
	var upgrades atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upgrades.Add(1) <= 2 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		muxHandler().ServeHTTP(w, r)
	}))
	defer srv.Close()
	host, port := srvAddr(t, srv)

	p := New(Config{
		Nodes:     repeatNodes(host, port, 2),
		SNI:       "example.com",
		Password:  "pw",
		Insecure:  true,
		MuxTarget: 1,
	})
	defer p.Close()
	p.Warm()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && p.liveCount() < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if p.liveCount() < 1 {
		t.Fatalf("no tunnel after the first failures were served (upgrades=%d): "+
			"warm-up gives up instead of retrying", upgrades.Load())
	}
}

// TestConcurrentDialStaysWithinTarget 钉住"请求路径也要守名额"。
//
// 背景：58ffc12 修了预热路径的超发，却漏了请求路径 —— `Pool.Dial` 直接调 redial，
// 完全不经过 warming 计数。断线瞬间上层同时涌进十几条连接、池又是空的，于是每个
// 请求各拨一条：实测 muxTarget=2 时池里能出现 6 条。每条常连隧道都按 DO 时长
// 计费，正是免费版最紧张的那笔额度。
func TestConcurrentDialStaysWithinTarget(t *testing.T) {
	srv := miniWSTLS(t)
	defer srv.Close()
	host, port := srvAddr(t, srv)

	p := New(Config{
		Nodes:     repeatNodes(host, port, 8),
		SNI:       "example.com",
		Password:  "pw",
		Insecure:  true,
		MuxTarget: 2,
	})
	defer p.Close()

	// 池是空的 —— 这正是会超发的时刻。八个请求一起进来。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Dial("example.com:443")
			if err == nil {
				_ = c.Close()
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(3 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		n = p.liveCount()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n > 2 {
		t.Errorf("live tunnels = %d for a target of 2: the request path is not counting "+
			"its dials, so a burst of connections multiplies the DO time bill", n)
	}
}

// TestIdleTrimActuallyShrinksThePool 钉住"空闲回收真的会收缩隧道数"。
//
// 这是一条对着文档写的测试。`limitations.md` 与代码注释一直写着"空闲期收缩到
// 一条"，而实际行为是：回收 -> Close -> onDead -> 无条件 TopUp -> 立刻补一条回来。
// 隧道数没变、额度没省，还多付一次握手和一次 DO 创建 —— 一项纯亏的额度保护。
//
// 现在的契约：空闲回收之后不补齐；要扩容由请求路径自己说（排进队列 = 需求超了）。
//
// 对端必须是**真的能拨通**的：否则"补齐的那次拨号失败"，池看起来缩了，而补齐
// 路径有没有被真正掐断根本没被验证到 —— 第一版就栽在这里，变异测试一跑才发现
// 这条用例压根抓不住它要抓的东西。
func TestIdleTrimActuallyShrinksThePool(t *testing.T) {
	srv := miniWSTLS(t)
	defer srv.Close()
	host, port := srvAddr(t, srv)

	p := New(Config{
		Nodes:         repeatNodes(host, port, 4),
		SNI:           "example.com",
		Password:      "pw",
		Insecure:      true,
		MuxTarget:     2,
		IdleTrimDelay: 40 * time.Millisecond,
	})
	defer p.Close()

	p.Warm()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && p.liveCount() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if p.liveCount() < 2 {
		t.Fatalf("warm-up produced %d tunnels, want 2", p.liveCount())
	}

	// 两条都还没接过流（备用状态），等第一条被空闲回收，然后盯住**之后**的峰值。
	//
	// 断峰值而不是断最后一刻的值：如果补齐没被掐断，池子会在 1 和 2 之间来回抖
	// （回收 -> 补一条 -> 新的那条再被回收），只看最后一刻有大约一半的运气落在 1。
	// 峰值是稳定的判据：补齐一旦发生，200ms 内就会把数字顶回 2。
	firstTrim := time.Now().Add(5 * time.Second)
	for time.Now().Before(firstTrim) && p.liveCount() > 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if p.liveCount() > 1 {
		t.Fatal("idle trimming never fired")
	}

	settle := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(settle) {
		if n := p.liveCount(); n > 1 {
			t.Fatalf("live tunnels = %d after the pool had shrunk to 1: trimming is being "+
				"undone by an immediate refill, so it saves no DO time quota while still "+
				"costing a handshake and a Durable Object each round", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := p.liveCount(); n < 1 {
		t.Errorf("live tunnels = %d: the last tunnel was reaped too, leaving the client "+
			"unable to browse without a reconnect", n)
	}
}
