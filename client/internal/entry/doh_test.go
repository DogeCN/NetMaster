package entry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestFromServerPrefersDoH 钉住新契约：入口解析**优先信 DoH**。
//
// 为什么必须优先：系统解析器会被投毒（实测 en.wikipedia.org → 31.13.94.41，Facebook
// 网段），而入口是整条链路里唯一没有第三方兜底的一环。这里用 localhost 当"系统解析
// 一定成功"的对照：DoH 给了答案时，绝不能因为系统先回就采用系统那份。
func TestFromServerPrefersDoH(t *testing.T) {
	old := resolveViaDoH
	defer func() { resolveViaDoH = old }()
	resolveViaDoH = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.7")}, nil
	}

	got := FromServer(context.Background(), "localhost")
	if len(got) != 1 || got[0].Addr != "203.0.113.7" {
		t.Fatalf("got %v; the DoH answer must win over the system resolver", got)
	}
}

// TestFromServerFallsBackToSystemResolver 是上一条的另一半：DoH 挂了就退系统解析，
// 而不是交白卷 —— "DoH 挂了"不该比"被投毒"更糟。
func TestFromServerFallsBackToSystemResolver(t *testing.T) {
	old := resolveViaDoH
	defer func() { resolveViaDoH = old }()
	resolveViaDoH = func(context.Context, string, string) ([]net.IP, error) {
		return nil, errors.New("doh down")
	}

	got := FromServer(context.Background(), "localhost")
	if len(got) == 0 {
		t.Fatal("DoH failed and the system resolver was not used; the entry pool would be empty")
	}
	for _, n := range got {
		if ip := net.ParseIP(n.Addr); ip == nil || !ip.IsLoopback() {
			t.Fatalf("got %v; want the system resolver's answer for localhost", n)
		}
	}
}

// rtFunc 让测试用一个假 RoundTripper 顶掉 fragClient。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestFetchSourceTriesECHFirstThenFallsBackToFragmentation 钉住入口源的取数顺序。
//
// 顺序是有理由的：三个源都发布并接受 ECH（实测 [2a] 全 OK），ECH 的 SNI 是**真加密**
// 且没有分片那约 400ms；但 ECH 依赖"发布配置 + 边缘接受 + DoH 拿得到"三件事同时成立，
// 任何一条不成立就必须退回分片 —— 这条用例模拟的正是"ECH 拿不到配置"的退路。
func TestFetchSourceTriesECHFirstThenFallsBackToFragmentation(t *testing.T) {
	// ① ECH 拿不到配置 → 不该走 ECH 客户端。
	oldFetch := fetchECHConfigFor
	oldClient := fragClient
	defer func() { fetchECHConfigFor = oldFetch; fragClient = oldClient }()

	echCalls := 0
	fetchECHConfigFor = func(string, string) ([]byte, error) {
		echCalls++
		return nil, errors.New("no ech config")
	}
	fragUsed := 0
	fragClient = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		fragUsed++
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("104.21.38.67:443#edge\n")),
			Header:     http.Header{},
		}, nil
	})}

	nodes, err := fetchSource(context.Background(), "https://ipdb.api.030101.xyz/?type=bestcf")
	if err != nil {
		t.Fatalf("fetchSource: %v", err)
	}
	if echCalls != 1 {
		t.Fatalf("ECH config was queried %d time(s), want 1 — the ECH path must be attempted first", echCalls)
	}
	if fragUsed != 1 {
		t.Fatalf("fragmentation was used %d time(s), want 1 — no ECH config means the fragmented path", fragUsed)
	}
	if len(nodes) != 1 || nodes[0].Addr != "104.21.38.67" {
		t.Fatalf("got %v, want the fragmented fetch's body to be parsed", nodes)
	}
}

// TestFetchSourceFallsBackWhenECHHandshakeFails 覆盖另一半：**拿到配置但握手失败**。
//
// 这条比上一条重要：配置存在不等于能用（crypto.cloudflare.com 就是有配置但边缘不认）。
// 假客户端让 ECH 那次必然失败（没有真的 ECH 服务端），断言退路仍然把源拉回来了。
func TestFetchSourceFallsBackWhenECHHandshakeFails(t *testing.T) {
	oldFetch := fetchECHConfigFor
	oldClient := fragClient
	defer func() { fetchECHConfigFor = oldFetch; fragClient = oldClient }()

	fetchECHConfigFor = func(string, string) ([]byte, error) {
		// 71 字节的假配置：足以让 ECH 路径真的去握手（然后失败），而不是提前退出。
		return make([]byte, 71), nil
	}
	fragUsed := 0
	fragClient = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		fragUsed++
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("104.21.38.67:443\n")),
			Header:     http.Header{},
		}, nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes, err := fetchSource(ctx, "https://ipdb.api.030101.xyz/?type=bestcf")
	if err != nil {
		t.Fatalf("fetchSource: %v", err)
	}
	if fragUsed != 1 {
		t.Fatalf("fragmentation was used %d time(s), want 1 — a failed ECH handshake must fall back", fragUsed)
	}
	if len(nodes) != 1 || nodes[0].Addr != "104.21.38.67" {
		t.Fatalf("got %v, want the fallback's body to be parsed", nodes)
	}
}

var _ = fmt.Sprintf // 保持 fmt 导入（上面的错误串在改动后可能不再用到）
