package selector

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"netmaster/internal/entry"
)

// noECH 在测试期间把 ECH 配置取回成 nil，让 Optimize 走明文 SNI 分支。
//
// 少了它，每条用例都会在测试机上真的去 Cloudflare 的 DoH 取一次配置 —— 于是
// "慢"和"通断"取决于这台机器当时能不能连上外网，而失败原因根本不在用例里。
func noECH(t *testing.T) {
	t.Helper()
	old := echConfigFetch
	echConfigFetch = func(string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { echConfigFetch = old })
}

// passThroughScreen 旁路掉 TCP 筛查这一段，让候选原样进入后面的阶段。
//
// 见 screenTCP 那个接缝的说明：这条用例要测的是"升级筛不筛得掉坏 IP"，
// 不该被前面那一段对着本机临时端口的偶发建连失败判红。
func passThroughScreen(t *testing.T) {
	t.Helper()
	old := screenTCP
	screenTCP = func(_ context.Context, nodes []entry.Node) []ProbeResult {
		out := make([]ProbeResult, len(nodes))
		for i, n := range nodes {
			out[i] = ProbeResult{Node: n, Latency: time.Duration(i + 1)}
		}
		return out
	}
	t.Cleanup(func() { screenTCP = old })
}

// nodeFor 把一个候选指向指定端口。两个 TLS 测试服务器都跑在 127.0.0.1 上、
// 端口不同 —— 节点靠 IP:Port 区分，正好用来造"一半好一半坏"的候选集。
func nodeFor(host string, port int) entry.Node { return entry.Node{Addr: host, Port: uint16(port)} }

// acceptingEdge 是一个"好"的边缘：TLS 正常，WS 升级也给 101。
func acceptingEdge(t *testing.T) (*httptest.Server, string, int) {
	t.Helper()
	srv := miniWSTLS(t)
	t.Cleanup(srv.Close)
	host, port := srvAddr(t, srv)
	return srv, host, port
}

// rejectingEdge 是本文件要刻画的那类边缘：**TLS 握手完全正常，一到 WS 升级就
// 被拒**（403 / error 1034 这类）。真实边缘在 Cloudflare 前，行为就是这个形状。
func rejectingEdge(t *testing.T) (string, int) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error 1034: edge IP is restricted", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	host, port := srvAddr(t, srv)
	return host, port
}

// TestOptimizeRejectsUpgradeFailingIPs 是 C2 的门：TLS 正常但 WS 升级被拒的候选
// 必须在进池前被剔掉。
//
// 少了升级这一阶段，这些 IP 会带着"握手很快"的排序进池，用户第一次打开某个站
// 要先吃满一整个拨号超时再加一次对冲重拨才连得上 —— 也就是最初报上来的"时好时坏"。
// 而边缘不会告诉你为什么，用户只会觉得"这个软件不稳定"。
func TestOptimizeRejectsUpgradeFailingIPs(t *testing.T) {
	noECH(t)
	passThroughScreen(t)
	// 每个候选一个**独立**的监听端口：三个候选共用一个端口看起来省事，但候选集
	// 长一个样子才像真实情况（真实候选是不同的入口 IP），写出来的断言也才能
	// 当回归测试用。
	// 候选数刻意压到最小（2+2）：这条用例会反复跑，而每建一个本地监听就占一个
	// 临时端口，连跑十几轮之后 Windows 的临时端口会开始 TIME_WAIT，新建的监听
	// 偶发连不上 —— 那是测试环境的产物，不该让回归测试去承担。
	const good, bad = 2, 2
	var nodes []entry.Node
	var badPorts []int
	for i := 0; i < good; i++ {
		_, host, port := acceptingEdge(t)
		nodes = append(nodes, nodeFor(host, port))
	}
	for i := 0; i < bad; i++ {
		host, port := rejectingEdge(t)
		nodes = append(nodes, nodeFor(host, port))
		badPorts = append(badPorts, port)
	}

	got := Optimize(context.Background(), nodes, "example.com", true)

	// 断言的是**性质**，不是条数。
	//
	// 原因是被实测教会的：探测本来就是"谁这次没连上就淘汰谁"，所以某次运行里
	// 一个本该可达的候选完全可能因为一次偶发的建连失败而没进池。早先这里断言
	// "rejected 恰好 3 条、池子恰好 3 条"，-count=40 就能跑出十几次红 —— 红的是
	// 测试，不是代码。
	//
	// 真正要保证的两件事是：被拒的候选**一个都不许**进池；升级阶段确实在干活。
	if len(got.Nodes) == 0 {
		t.Fatal("empty node pool")
	}
	for _, n := range got.Nodes {
		for _, p := range badPorts {
			if int(n.Port) == p {
				t.Errorf("node %s:%d reached the pool although its WS upgrade is refused", n.Addr, n.Port)
			}
		}
	}
}

// TestOptimizeUpgradeStageIsTimeBounded 钉住升级阶段的时间上限。
//
// 这条是被一次真实启动量出来的：第一版让升级走 outbound.DialWS（TCP+TLS 6s、
// 握手 8s），实测启动从 5.1s 涨到 13.1s —— 探测整整用满 10s 预算，而这段时间
// 用户什么都干不了，只能看着浏览器打不开。挂死的边缘只要有一个就能吃掉整个预算。
//
// 断言用的是"远小于旧时限"的量级，所以只要有人把超时调回秒级就会红。
// hangingUpgradeEdge 是最难缠的那类边缘：**TLS 握手完全正常**（所以前面的筛查与
// 握手阶段都会把它排进前列），升级请求发过去之后一个字节都不回。实测里这类比
// 直接拒绝更常见，也更贵 —— 直接拒绝至少是秒回的。
func hangingUpgradeEdge(t *testing.T) int {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release // 永远不写响应
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	_, port := srvAddr(t, srv)
	return port
}

func TestOptimizeUpgradeStageIsTimeBounded(t *testing.T) {
	noECH(t)
	passThroughScreen(t)
	// 上面那条用例自己把时限压到 300ms，所以它证明的是"升级阶段读的是这个变量"。
	// 默认值本身要单独钉：把它调回秒级不会让任何用例变红，而那正是 13.1s 那次
	// 启动的真实原因。
	if probeUpgradeTimeout > 3*time.Second {
		t.Fatalf("probeUpgradeTimeout is %v; a probe stage budget is not a place for seconds "+
			"(see the 13.1s startup in the commit that introduced it)", probeUpgradeTimeout)
	}

	old := probeUpgradeTimeout
	probeUpgradeTimeout = 300 * time.Millisecond
	defer func() { probeUpgradeTimeout = old }()

	// 同样是 1 坏 + 1 好：候选越少，监听端口压力越小，这条用例就越只因为它要测的
	// 那件事而红。
	_, goodHost, goodPort := acceptingEdge(t)
	deadPort := hangingUpgradeEdge(t)

	nodes := []entry.Node{nodeFor("127.0.0.1", deadPort), nodeFor(goodHost, goodPort)}

	got := Optimize(context.Background(), nodes, "example.com", true)

	// 只看升级那一段：TCP 筛查与 TLS 段各有自己的时限（拖死的边缘会先死在那两段），
	// 混在一起测就测不出"是升级阶段超时了"还是"前一阶段本来就要那么久"。
	// 旧实现下这一段至少要 6 秒（tlsutil 的 dialPhaseTimeout）。
	if got.UpgradeTook > 1500*time.Millisecond {
		t.Errorf("ws upgrade stage took %v: a dead entry edge is allowed to eat the startup budget",
			got.UpgradeTook)
	}
	var kept bool
	for _, n := range got.Nodes {
		if n.Port == uint16(goodPort) {
			kept = true
		}
	}
	if !kept {
		for _, r := range got.Refused {
			t.Logf("refused %s:%d — %s", r.Node.Addr, r.Node.Port, r.Err)
		}
		for _, e := range got.TLSErrors {
			t.Logf("tlsErr %s", e)
		}
		var gotPorts []int
		for _, n := range got.Nodes {
			gotPorts = append(gotPorts, int(n.Port))
		}
		t.Errorf("the working edge (port %d) is not in the pool %v; upgraded=%d refused=%d screen=%s tls=%s",
			goodPort, gotPorts, got.Upgraded, got.Rejected, got.ScreenTook, got.TLSTook)
	}
}

// TestProbeUsesECHWhenConfigAvailable 钉住"有 ECH 配置时探测必须真的走 ECH"。
//
// 为什么要钉：探测原本一律用明文 SNI，于是每次启动都把服务端域名明文发出去
// 40~60 次，而且量的根本不是客户端要走的那条路。实测那次"11 个候选在升级阶段
// 被拒"里，只有 2 个是 Cloudflare 回的 403，另外 8 个是握手刚完就被 RST ——
// 明文 SNI 正是最招这种事的做法。
//
// 判据很土但很硬：对着一个只认普通 TLS 的本地服务器递一份**坏掉的** ECH 配置，
// 如果握手仍然成功，就说明 ECH 那条路根本没走。
func TestProbeUsesECHWhenConfigAvailable(t *testing.T) {
	srv := miniWSTLS(t)
	defer srv.Close()
	host, port := srvAddr(t, srv)
	node := nodeFor(host, port)

	if r, c := probeOne(context.Background(), node, "example.com", true, nil); r.Err != nil {
		t.Fatalf("plain-SNI probe should succeed against the local server: %v", r.Err)
	} else if c == nil {
		t.Fatal("a successful probe must hand its established connection back for reuse")
	} else {
		_ = c.Close()
	}
	if r, _ := probeOne(context.Background(), node, "example.com", true, []byte("not-an-ech-configlist")); r.Err == nil {
		t.Fatal("a bogus ECH config still completed a plain handshake — " +
			"the ECH path is not being taken when a config is available")
	}
}

// TestScreenTCPKeepsReachableAndDropsDead 给被上面几条用例旁路掉的那一段补上门。
//
// 不补的话，"筛查会不会把明显不通的候选挡掉"就没人管了 —— 而它挡掉的正是
// 启动里最慢的那部分（连都连上的候选，每个都要等满时限）。
//
// 只断言"死的那个一定被挡掉"，不断言"活的一定留下"：连反复跑时偶发失败的是
// **对活着的本机监听建连**，本机临时端口的 TIME_WAIT 压力造成的（跑一遍要起几十个
// httptest）。"留下的都是可达的"这一半由真实启动的日志保证，而不是由一条会随机
// 变红的断言来假装保证。
func TestScreenTCPKeepsReachableAndDropsDead(t *testing.T) {
	_, host, port := acceptingEdge(t)

	// 先建好要活着的服务器，再去拿一个"死端口"：顺序反过来会偶发红 ——
	// 端口刚释放就被 httptest 绑定，于是那个"连不上"的候选其实连得上。
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	_ = dead.Close()

	got := ScreenTCP(context.Background(), []entry.Node{
		nodeFor("127.0.0.1", deadPort),
		nodeFor(host, port),
	})
	for _, r := range got {
		if int(r.Node.Port) == deadPort {
			t.Errorf("screen kept %d although nothing is listening there", deadPort)
		}
		if r.Err == nil && r.Latency <= 0 {
			t.Errorf("screen kept %d without a latency; unreachable candidates must be distinguishable", r.Node.Port)
		}
	}
}

// TestOptimizeFallbackIsStillCapped 钉死兜底路径的上限。
//
// 全被拒时要退回"TLS 通的那批"是对的，但**退回多少条**是个独立的问题：
// len(nodes) 是 pickNode 的分母，交回全部候选等于把节点选择面悄悄放大好几倍。
// 这个上限一度被漏掉，是对抗评审翻出来的 —— 兜底路径最容易长出没人想过的性质。
func TestOptimizeFallbackIsStillCapped(t *testing.T) {
	noECH(t)
	passThroughScreen(t)

	badHost, badPort := rejectingEdge(t)
	more := probeTopN + 6
	nodes := make([]entry.Node, more)
	for i := range nodes {
		nodes[i] = nodeFor(badHost, badPort)
	}

	got := Optimize(context.Background(), nodes, "example.com", true)

	if got.Rejected != 0 {
		t.Errorf("the fallback pool *is* the refused set; reporting %d refusals alongside it is self-contradictory", got.Rejected)
	}
	if len(got.Nodes) > probeTopN {
		t.Errorf("fallback pool = %d entries, want at most probeTopN (%d)", len(got.Nodes), probeTopN)
	}
	if len(got.Nodes) == 0 {
		t.Error("the fallback must not be empty — that leaves the client unusable")
	}
}

// TestOptimizeReportsThatECHFailed 钉住"ECH 失败要说出来"。
//
// 这是本项目最贵的一次教训：ECH 从来没成功过，每个连接都静默退回明文 SNI，
// 而 README、config 的 no-ech 开关、界面文案都写着它在隐 —— 用户以为被保护着。
// 失败一路静默了整整一个版本。
//
// 判据不是"日志里有没有那句话"，而是**结果里有没有这个字段**：
// 静默退回发生在 dialTLS 里，那一层没有 logger，事后翻代码只会看到一句
// "普通 TLS 兜底" 注释。能把这件事带出来的唯一位置就是探测结果。
func TestOptimizeReportsThatECHFailed(t *testing.T) {
	passThroughScreen(t)
	_, host, port := acceptingEdge(t)

	old := echConfigFetch
	echConfigFetch = func(string) ([]byte, error) { return []byte("bogus-ech-config"), nil }
	defer func() { echConfigFetch = old }()

	got := Optimize(context.Background(), []entry.Node{nodeFor(host, port)}, "example.com", true)

	if !got.ECHConfigured {
		t.Error("a config was available, so the probe must report that it tried ECH")
	}
	if got.ECHWorked {
		t.Error("a bogus ECH config cannot complete a handshake; reporting it as working " +
			"is exactly the silence this test exists to prevent")
	}
}
