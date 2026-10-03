package selector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"netmaster/internal/entry"
)

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
	_, goodHost, goodPort := acceptingEdge(t)
	badHost, badPort := rejectingEdge(t)

	var nodes []entry.Node
	for i := 0; i < 3; i++ {
		nodes = append(nodes, nodeFor(goodHost, goodPort))
		nodes = append(nodes, nodeFor(badHost, badPort))
	}

	got := Optimize(context.Background(), nodes, "example.com", true)

	if got.Rejected != 3 {
		t.Errorf("rejected = %d, want 3 (the upgrade-refused candidates)", got.Rejected)
	}
	if len(got.Nodes) != 3 {
		t.Fatalf("node pool = %d entries, want 3", len(got.Nodes))
	}
	for _, n := range got.Nodes {
		if n.Port == uint16(badPort) {
			t.Fatalf("node %s:%d reached the pool although its WS upgrade is refused", n.Addr, n.Port)
		}
	}
}

// TestOptimizeKeepsPoolWhenAllRejected 兜底：候选全都在升级阶段被拒时，仍然要交
// 一个非空池。空池等于"客户端完全不可用"，而"让拨号层再试一次"至少还有救。
func TestOptimizeKeepsPoolWhenAllRejected(t *testing.T) {
	badHost, badPort := rejectingEdge(t)

	nodes := []entry.Node{nodeFor(badHost, badPort), nodeFor(badHost, badPort)}
	got := Optimize(context.Background(), nodes, "example.com", true)

	if got.Rejected == 0 {
		t.Fatal("these candidates should have been rejected at the upgrade stage")
	}
	if len(got.Nodes) == 0 {
		t.Fatal("an empty node pool makes the client unusable; fall back to the TLS-verified list")
	}
}

// TestOptimizePassesThroughGoodEdges 对照路径：一个都别误伤。
func TestOptimizePassesThroughGoodEdges(t *testing.T) {
	_, host, port := acceptingEdge(t)

	nodes := []entry.Node{nodeFor(host, port), nodeFor(host, port)}
	got := Optimize(context.Background(), nodes, "example.com", true)

	if got.Rejected != 0 {
		t.Errorf("rejected = %d, want 0", got.Rejected)
	}
	if len(got.Nodes) != 2 {
		t.Fatalf("node pool = %d entries, want 2", len(got.Nodes))
	}
}

// hangingUpgradeEdge 是最难缠的那类边缘：**TLS 握手完全正常**（所以第一阶段探测
// 会把它排进前列），升级请求发过去之后一个字节都不回。实测里这类比直接拒绝更常见，
// 也更贵 —— 直接拒绝至少是秒回的。
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

// TestOptimizeUpgradeStageIsTimeBounded 钉住升级阶段的时间上限。
//
// 这条是被一次真实启动量出来的：第一版让升级走 outbound.DialWS（TCP+TLS 6s、
// 握手 8s），实测启动从 5.1s 涨到 13.1s —— 探测整整用满 10s 预算，而这段时间
// 用户什么都干不了，只能看着浏览器打不开。挂死的边缘只要有一个就能吃掉整个预算。
//
// 断言用的是"远小于旧时限"的量级，所以只要有人把超时调回秒级就会红。
func TestOptimizeUpgradeStageIsTimeBounded(t *testing.T) {
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

	_, goodHost, goodPort := acceptingEdge(t)
	deadPort := hangingUpgradeEdge(t)

	var nodes []entry.Node
	for i := 0; i < 3; i++ {
		nodes = append(nodes, nodeFor("127.0.0.1", deadPort))
	}
	nodes = append(nodes, nodeFor(goodHost, goodPort))

	start := time.Now()
	got := Optimize(context.Background(), nodes, "example.com", true)
	took := time.Since(start)

	// 旧实现下这一波至少要 6 秒（tlsutil 的 dialPhaseTimeout）。给 2.5s 上限。
	if took > 2500*time.Millisecond {
		t.Errorf("probe took %v: a dead entry edge is allowed to eat the whole startup budget", took)
	}
	var kept bool
	for _, n := range got.Nodes {
		if n.Port == uint16(goodPort) {
			kept = true
		}
	}
	if !kept {
		t.Error("the one working edge was dropped because dead ones timed out slowly")
	}
}
