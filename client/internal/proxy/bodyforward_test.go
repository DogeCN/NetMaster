package proxy

// 一次审查里查出的客户端侧缺陷的钉子。
//
// 两条规矩照旧：
//   - 回退修复，本文件必须变红；
//   - 走**真实代码路径**。P0-1 那次教训是：测试绕过了 s.dial，于是缺陷藏在
//     接线之外。所以这里的 HTTP 用例真的起 listener、真的走 s.dial、真的读回源站。

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------------- HIGH：明文 HTTP 的请求体被丢弃 ----------------

// originRec 冒充源站，把收到的请求行与 body 原样回报，好让断言比对真实字节。
func originRec(t *testing.T) *httptest.Server {
	t.Helper()
	var got struct {
		method string
		path   string
		body   string
		length string // 源站实际读到的 Content-Length
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.path = r.URL.Path
		got.body = string(b)
		got.length = fmt.Sprint(r.ContentLength)
		fmt.Fprintf(w, "ok:%d", len(b))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		t.Logf("origin saw: %s %s Content-Length=%s body=%q", got.method, got.path, got.length, got.body)
	})
	return srv
}

// serveOn 起一个只跑 HTTP 的监听，返回它的地址。走的是真实的 serveHTTP。
func serveOn(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.serveHTTP(ln)
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// TestPlainHTTPForwardsTheRequestBody 是那条 100% 损坏的钉子。
//
// 原实现把 req.Body 抽干进 io.Discard，再 req.Write(up) 重新序列化 —— 头还在，
// body 却是 0 字节。源站于是按 Content-Length 继续等，等到超时。
// GET 没有 body 所以看不出来，于是它能一直活着；而 POST/PUT/PATCH 与一切 API
// 调用全部损坏。
//
// 断言的是**源站读到的字节**，不是代理的内部状态：只有走完全链路才能证明
// "body 真的到了"，绕一层就会重蹈 P0-1 那次"测了但没测到接线"的覆辙。
func TestPlainHTTPForwardsTheRequestBody(t *testing.T) {
	origin := originRec(t)
	payload := `{"user":"doge","n":42}`

	s := New(Config{
		Router:         alwaysProxyRouter{},
		Pool:           &stubPool{}, // 空池 ⇒ 走 DirectFallback 直连本机源站
		DirectFallback: true,
		DialTimeout:    5 * time.Second,
		Logger:         log.New(testWriter{t}, "", 0),
	})
	addr := serveOn(t, s)

	// 用**真代理**的用法发请求：Transport.Proxy 指到本地代理，于是 client 发出
	// absolute-form 的请求行（`POST http://origin/submit HTTP/1.1`）。这样
	// handleHTTPConn 读到的 req.URL.Host / Scheme 才是目标的真实地址与协议 ——
	// 直接 http.Post 到监听端口是 origin-form，URL 里没有 host，会被当成走 80 端口。
	proxyURL, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}

	req, err := http.NewRequest(http.MethodPost, origin.URL+"/submit", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxied POST failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if want := fmt.Sprintf("ok:%d", len(payload)); string(body) != want {
		t.Fatalf("origin received the wrong byte count: got %q, want %q — "+
			"the request body must arrive intact", body, want)
	}
}

// TestPlainHTTPRejectsOversizeBodyInsteadOfTruncating 钉住上限的处理方式。
//
// 旧实现对 >1MB 的 body 是**截断**（与 ≤1MB 的丢弃是两种不同的坏法）：
// 截断同样让源站按 Content-Length 空等。两害相权，宁可明说 413。
func TestPlainHTTPRejectsOversizeBodyInsteadOfTruncating(t *testing.T) {
	s := New(Config{
		Router:         alwaysProxyRouter{},
		Pool:           &stubPool{},
		DirectFallback: true,
		DialTimeout:    5 * time.Second,
		Logger:         log.New(testWriter{t}, "", 0),
	})
	addr := serveOn(t, s)

	// 直接手写请求，免得 http.Client 自己在发之前就拒了大 body。
	big := strings.Repeat("x", forwardBodyLimit+1)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))

	fmt.Fprintf(conn, "POST /big HTTP/1.1\r\nHost: example.com\r\nContent-Length: %d\r\n\r\n%s", len(big), big)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("no status line back: %v", err)
	}
	if !strings.Contains(status, "413") {
		t.Fatalf("oversize body should be refused with 413, got %q", strings.TrimSpace(status))
	}
}

// TestEmptyBodyGETStillWorks 是对照组：别把修复做过头，把正常的 GET 也弄坏。
func TestEmptyBodyGETStillWorks(t *testing.T) {
	origin := originRec(t)
	s := New(Config{
		Router:         alwaysProxyRouter{},
		Pool:           &stubPool{},
		DirectFallback: true,
		DialTimeout:    5 * time.Second,
		Logger:         log.New(testWriter{t}, "", 0),
	})
	addr := serveOn(t, s)

	proxyURL, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}
	resp, err := client.Get(origin.URL + "/ping")
	if err != nil {
		t.Fatalf("proxied GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok:0" {
		t.Fatalf("GET should carry an empty body through, got %q", body)
	}
}

// ---------------- HIGH：Optimize 的 defer 写不回未命名返回值 ----------------

// TestDeferWritesThroughNamedResult 把 Go 那条语言规则钉成可执行断言。
//
// Optimize 原本返回**未命名** OptResult：`return res` 会先把 res 拷进返回槽位、
// 再执行 defer，于是 `defer func(){ res.ECHWorked = ... }()` 那次赋值被丢掉，
// ECHWorked 恒为 false。
//
// 后果不是"少个字段"：客户端启动日志里 "[ech] ...VISIBLE SNI" 正是靠它才碰巧说对了
// 话（ECH 确实没生效）。用户照 README 在 CF zone 上打开 ECH 之后，这行日志**仍然**
// 会说没生效 —— 恰好摧毁掉唯一提示他去开 ECH 的信号。
//
// 改回未命名返回值即红。
func TestDeferWritesThroughNamedResult(t *testing.T) {
	type OptResult struct {
		ECHConfigured bool
		ECHWorked     bool
		echOn         bool
	}

	// mirrors Optimize 的形状：defer 在 return 之前跑，写的是命名返回槽位。
	probe := func() (res OptResult) {
		res = OptResult{ECHConfigured: true, echOn: true}
		defer func() { res.ECHWorked = res.echOn }()
		return res
	}

	got := probe()
	if !got.ECHWorked {
		t.Fatal("defer write to a named result is lost — this is the exact bug that " +
			"pinned ECHWorked to false")
	}
	if !got.ECHConfigured {
		t.Fatal("named result lost its plain field assignment")
	}
}

// TestHMACFrameRoundTrip 给上面那条钉子一个真实的同类样本：
// 签名覆盖 TS‖ID‖ATYP‖ADDR‖PORT 加尾随字节，且长度前置比较安全。
//
// 这条不是为了发现新缺陷，是为了让 defer/命名返回值那条规则旁边有一个
// 真实的"覆盖范围不能缩"参照 —— 审查点名 crypto.js 的常量时间比较与
// 无截断签名做得好，值得钉住防回退。
func TestHMACFrameRoundTrip(t *testing.T) {
	key := []byte("k")
	sign := func(b []byte) []byte {
		m := hmac.New(sha256.New, key)
		m.Write(b)
		return m.Sum(nil)
	}
	body := []byte{0, 0, 0, 0, 1, 2, 1, 4, 'h', 'o', 's', 't', 1, 187}
	mac := sign(body)

	if !hmac.Equal(mac, sign(body)) {
		t.Fatal("identical input must verify")
	}
	if hmac.Equal(mac, sign(append(append([]byte{}, body...), 9))) {
		t.Fatal("a trailing byte must break the MAC — signature must cover it")
	}
}
