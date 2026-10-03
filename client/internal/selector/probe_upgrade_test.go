package selector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
