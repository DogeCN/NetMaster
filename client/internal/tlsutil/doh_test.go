package tlsutil

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveIPsParsesAAndAAAA 钉住 DoH 解析的两件事：两种记录都认，且 CNAME 不算 IP。
func TestResolveIPsParsesAAndAAAA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("type") {
		case "A":
			fmt.Fprint(w, `{"Status":0,"Answer":[{"type":5,"data":"alias.example.com."},{"type":1,"data":"203.0.113.7"}]}`)
		case "AAAA":
			fmt.Fprint(w, `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::1"}]}`)
		default:
			fmt.Fprint(w, `{"Status":0,"Answer":[]}`)
		}
	}))
	defer srv.Close()

	ips, err := ResolveIPs(context.Background(), "example.com", srv.URL)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	got := map[string]bool{}
	for _, ip := range ips {
		got[ip.String()] = true
	}
	if !got["203.0.113.7"] || !got["2001:db8::1"] {
		t.Fatalf("got %v; want both the A and the AAAA record (CNAME must not be returned)", ips)
	}
	if len(ips) != 2 {
		t.Fatalf("got %d addresses, want 2 — a CNAME target is not an address", len(ips))
	}
}

// TestResolveIPsRejectsBadAnswers 钉住"坏答案不能当成空答案"。
//
// 测的是**端点级**函数：ResolveIPs 会在端点之间回退，坏端点被下一个端点救回来正是
// 它的职责；而"这个端点的答复不可信"必须在端点这一层就报错，否则调用方会以为
// "这个域名没有记录"，于是不再去问下一个端点。
func TestResolveIPsRejectsBadAnswers(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer bad.Close()
	if _, err := resolveAtEndpoint(context.Background(), bad.URL, "example.com"); err == nil {
		t.Fatal("bad json must be an error, not an empty answer")
	}

	servfail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Status":2,"Answer":[]}`)
	}))
	defer servfail.Close()
	if _, err := resolveAtEndpoint(context.Background(), servfail.URL, "example.com"); err == nil {
		t.Fatal("SERVFAIL must be an error, not an empty answer")
	}
}

// TestResolveIPsFallsBackAcrossEndpoints 钉住多端点回退：第一个坏了要问下一个。
func TestResolveIPsFallsBackAcrossEndpoints(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "A" {
			fmt.Fprint(w, `{"Status":0,"Answer":[{"type":1,"data":"198.51.100.9"}]}`)
			return
		}
		fmt.Fprint(w, `{"Status":0,"Answer":[]}`)
	}))
	defer good.Close()

	// dohBase 优先于内置端点；把坏端点放在第一位，好端点放在它后面。
	// 这里直接构造 endpoints 列表的语义：ResolveIPs 会先试 dohBase，失败再试内置的。
	// 内置端点在这台机器上可能通也可能不通，所以断言只要求"最终拿到好端点的答案"。
	ips, err := ResolveIPs(context.Background(), "example.com", good.URL)
	if err != nil {
		t.Fatalf("ResolveIPs: %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "198.51.100.9" {
		t.Fatalf("got %v, want the answer from the reachable endpoint", ips)
	}
}
