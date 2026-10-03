package selector

import (
	"net/http/httptest"
	"net/url"
	"strconv"
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
