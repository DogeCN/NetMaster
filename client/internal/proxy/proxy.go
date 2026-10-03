package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"netmaster/internal/profile"
	"netmaster/internal/rules"
)

// Router 是分流决策器：给定 host:port 返回直连还是代理。rules.Router 实现；
// 测试可注入替身。
type Router interface {
	Match(host string, port uint16) rules.Action
}

// Exiter 是出口选择器需要提供的最小能力面：分流决策（DialAuto 的 direct 返回值）、
// 代理建连与"直连被阻断后改走代理"。selector.Pool 实现；测试可注入替身。
type Exiter interface {
	Len() int
	DialAuto(host string) (net.Conn, bool, error)
	Dial(host string) (net.Conn, error)
	RetryProxy(host string) (net.Conn, error)
	NoteProxyFailure(host string)
}

// Config 代理服务配置。
type Config struct {
	// HTTPAddr 监听的 HTTP 代理地址，如 127.0.0.1:8080
	HTTPAddr string
	// SocksAddr 监听的 SOCKS5 代理地址，如 127.0.0.1:1080
	SocksAddr string
	// Router 分流决策
	Router Router
	// Pool 出口选择器（直连/代理决策与建连）。
	Pool Exiter
	// DirectFallback 路由未覆盖或节点池为空时是否直连（默认 true）
	DirectFallback bool
	// RetryViaProxy 在"尝试性直连"被判定阻断后改用代理重连（见 relayWithReplay）。
	// 默认走 Pool.RetryProxy；测试可注入替身。
	RetryViaProxy func(host string) (net.Conn, error)
	// DirectDial 分片直连重试时的拨号（见 tryFragmentedDirect）。
	// 留空则用 net.DialTimeout；测试可注入替身。
	DirectDial func(host string) (net.Conn, error)
	// IsHTTPS 判定一个 CONNECT 目标是否走"首段探测 + 重放"。留空 = 只认 443。
	//
	// 为什么留这个口子：测试无法在 127.0.0.1 上占用 443，而这一整条链路（分片、
	// 重放、改道）只在 HTTPS 上启用。没有它，端到端用例就只能去连真实的目标 ——
	// 那是把单元测试变成网络测试。
	IsHTTPS func(host string) bool
	// Trace 分段耗时采集器。留空即不采集（internal/profile.Trace 的零值是
	// nil，全部方法都是空操作），因此热路径上不需要任何 if。
	//
	// 装它的理由：改道与分片这些路径只在**出事时**才留下一行日志，成功的那次
	// 完全不留痕迹。于是"正常情况到底花了多久"无从回答，而那恰恰是优化要看的地方。
	Trace *profile.Trace
	// Logger
	Logger *log.Logger
	// DialTimeout 单次建连超时
	DialTimeout time.Duration
}

// Server 本地混合代理（HTTP + SOCKS5）。
type Server struct {
	cfg     Config
	ln      net.Listener
	sn      net.Listener
	wg      sync.WaitGroup
	closing chan struct{}
	once    sync.Once
}

// New 创建代理服务。
func New(cfg Config) *Server {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 15 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Server{cfg: cfg, closing: make(chan struct{})}
}

// Start 启动 HTTP 与 SOCKS5 监听。
func (s *Server) Start() error {
	if s.cfg.HTTPAddr != "" {
		ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
		if err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
		s.ln = ln
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveHTTP(ln)
		}()
		s.cfg.Logger.Printf("[proxy] HTTP  proxy on %s", s.cfg.HTTPAddr)
	}
	if s.cfg.SocksAddr != "" {
		ln, err := net.Listen("tcp", s.cfg.SocksAddr)
		if err != nil {
			s.Close()
			return fmt.Errorf("socks listen: %w", err)
		}
		s.sn = ln
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveSocks(ln)
		}()
		s.cfg.Logger.Printf("[proxy] SOCKS5 proxy on %s", s.cfg.SocksAddr)
	}
	return nil
}

// Addrs 返回实际监听地址（可能含 :0 分配的实际端口）。
func (s *Server) Addrs() (httpAddr, socksAddr string) {
	if s.ln != nil {
		httpAddr = s.ln.Addr().String()
	}
	if s.sn != nil {
		socksAddr = s.sn.Addr().String()
	}
	return
}

// Close 关闭服务。
func (s *Server) Close() error {
	s.once.Do(func() { close(s.closing) })
	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	if s.sn != nil {
		_ = s.sn.Close()
	}
	return err
}

// aliveCounter 是"池里还有几个没被判死的节点"的可选能力面。selector.Pool 实现。
//
// 为什么用可选接口而不是加进 Exiter：Exiter 是导出契约，加方法会波及所有实现方；
// 而这里的默认答案（退化成 Len()）对老实现是完全正确的老行为，类型断言拿不到就
// 退回它，不需要任何人配合。proxy 里 FragDirecter 也是同一个套路。
type aliveCounter interface {
	Alive() int
}

// dial 按路由决策建连。
//
// 返回的 tentativeDirect 表示"这是一次尝试性的直连"：TCP 连上了，但不代表这个站
// 真能直连（GFW 常在看到 TLS SNI 后才发 RST）。调用方应据此决定要不要走
// relayWithReplay。规则明确要求直连的（比如局域网、.cn 名单）不算尝试性 ——
// 那是用户自己写的规则，失败就该失败，不由我们替他改道。
func (s *Server) dial(host string) (conn net.Conn, tentativeDirect bool, err error) {
	_, portStr, splitErr := net.SplitHostPort(host)
	port := uint16(0)
	if splitErr == nil {
		if n, perr := strconv.ParseUint(portStr, 10, 16); perr == nil {
			port = uint16(n)
		}
	}
	action := rules.Proxy
	if s.cfg.Router != nil {
		action = s.cfg.Router.Match(host, port)
	}
	switch action {
	case rules.Direct:
		c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
		return c, false, derr
	default: // proxy
		if !s.hasUsableExit() {
			if s.cfg.DirectFallback {
				// 这条分支**就是**"尝试性直连"的定义：我们没有把握它通，
				// 只是没有可用的出口了才退回来试。所以 tentativeDirect 必须为 true
				// —— 否则调用方不会走 relayWithReplay，整条"被阻断→分片→改道"的
				// 链路就一次都不会执行（这正是它此前一直惰性的原因）。
				c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
				return c, derr == nil, derr
			}
			return nil, false, errors.New("proxy action but no usable exit")
		}
		c, derr := s.cfg.Pool.Dial(host)
		return c, false, derr
	}
}

// hasUsableExit 判断代理侧还有没有可用的出口。
//
// 判据是"没被判死的节点数"而不是"池子里的节点数"（见 selector.Pool.Alive 的
// 说明）：池里躺着一批死节点时，Len() 依然大于 0，于是直连兜底永远不触发。
func (s *Server) hasUsableExit() bool {
	if s.cfg.Pool == nil {
		return false
	}
	if a, ok := s.cfg.Pool.(aliveCounter); ok {
		return a.Alive() > 0
	}
	return s.cfg.Pool.Len() > 0
}

// httpsish 判定目标是否走"首段探测 + 重放"这条链路。见 Config.IsHTTPS。
func (s *Server) httpsish(host string) bool {
	if s.cfg.IsHTTPS != nil {
		return s.cfg.IsHTTPS(host)
	}
	return isHTTPSPort(host)
}

// relay 双向转发，直到任一侧关闭。
func (s *Server) relay(a, b net.Conn, _ string) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src) //nolint:errcheck
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() //nolint:errcheck
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// forwardBodyLimit 是明文 HTTP 转发时允许的请求体上限。
//
// 超限返回 413，而不是截断后照发 —— 截断会让源站按 Content-Length 空等，超时挂死。
const forwardBodyLimit = 1 << 20

// forwardDrainLimit 是回 413 之前愿意替客户端抽干的字节数。
//
// 不是无限抽：不然"发一个不读完的超大 body"就能把连接占住不放。超出这个量就直接
// 关掉，那一跳客户端会看到连接重置 —— 它本来也在越界，可以接受。
const forwardDrainLimit = 4 << 20

// drainGrace 是上面那次抽干的总时限，防止慢速客户端把代理的 goroutine 钉住。
const drainGrace = 5 * time.Second

// ---------------- HTTP ----------------

func (s *Server) serveHTTP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closing:
				return
			default:
			}
			continue
		}
		go s.handleHTTPConn(c)
	}
}

func (s *Server) handleHTTPConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))

	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{}) // 握手后不限时

	// CONNECT
	if req.Method == http.MethodConnect {
		host := req.Host
		if host == "" {
			host = req.URL.Host
		}
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, "443")
		}
		s.tunnel(c, host)
		return
	}

	// 普通 HTTP：需转发到目标。我们把整个请求当不透明体，先直连/代理目标再写出去。
	// 简化：构造目标 host
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if host == "" {
		httpErr(c, 400, "bad request")
		return
	}
	// ⚠️ 这里**不能**预先把 body 抽干。原来两个分支都有
	// `io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))`，把请求体读掉扔进垃圾桶，
	// 随后 req.Write(up) 又照着 req 重新序列化 —— Content-Length 头还在，body 却是
	// 0 字节。源站于是按声明的长度继续等，等到超时。
	//
	// GET 没有 body 所以看不出来，于是它能一直活着；而 POST/PUT/PATCH 与一切 API
	// 调用 100% 损坏。（顺带：LimitReader 对 >1MB 的 body 是**截断**，与 ≤1MB 的
	// **丢弃**是两种不同的坏法。）
	//
	// 现在只做上限判定：超限就明说 413，而不是发一个长度对不上的请求出去。
	//
	// 413 之前必须先把 body 抽干：带着未读的接收缓冲关连接，TCP 会发 RST，
	// 客户端收到的是"连接被重置"而不是 413 —— 那就等于没明说。抽干量另有上限，
	// 不然恶意客户端能靠"发一个不读完的超大 body"把这条连接占住。
	if req.ContentLength > forwardBodyLimit {
		c.SetReadDeadline(time.Now().Add(drainGrace))
		_, _ = io.CopyN(io.Discard, req.Body, forwardDrainLimit)
		c.SetReadDeadline(time.Time{})
		httpErr(c, 413, "request body too large")
		return
	}
	if req.URL.Port() == "" {
		// 原样保留 scheme 默认端口
		if req.URL.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	up, _, err := s.dial(host)
	if err != nil {
		httpErr(c, 502, "dial error")
		return
	}
	defer up.Close()

	// 用新的 ReadRequest 结果重新序列化，保持原 method/path/header/body
	if err := req.Write(up); err != nil {
		return
	}
	s.relay(c, up, host)
}

// tunnel 处理 CONNECT：连上后回 200 再双向转发。
//
// 若这次连的是"尝试性直连"的 HTTPS，走 relayWithReplay —— 它会把选择推迟到
// 首个数据包回来之后再定，以便在被 GFW 阻断时无缝改走代理。
// 代理路径的 HTTPS 走 relayWithProxyReplay —— 与直连侧对称：隧道已建立但不等于
// 这跳真能用（worker 内联选中继，首次可能踩到"TCP 能通但不干活"的中继），
// 上游零字节即断时换出口重放一次，浏览器无感。
func (s *Server) tunnel(client net.Conn, host string) {
	up, tentativeDirect, err := s.dial(host)
	if err != nil {
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\n\r\n")

	if tentativeDirect && s.httpsish(host) {
		s.relayWithReplay(client, up, host)
		return
	}
	if !tentativeDirect && s.cfg.Pool != nil && s.httpsish(host) {
		s.relayWithProxyReplay(client, up, host)
		return
	}
	s.relay(client, up, host)
}

// ---------------- SOCKS5 ----------------

func (s *Server) serveSocks(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-s.closing:
				return
			default:
			}
			continue
		}
		go s.handleSocksConn(c)
	}
}

func (s *Server) handleSocksConn(c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	host, err := socksHandshake(c)
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	up, tentativeDirect, err := s.dial(host)
	if err != nil {
		// reply: 失败
		c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) //nolint:errcheck
		return
	}
	defer up.Close()
	// reply 成功（bound addr 填 0.0.0.0:0）
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) //nolint:errcheck
	if tentativeDirect && s.httpsish(host) {
		s.relayWithReplay(c, up, host)
		return
	}
	if !tentativeDirect && s.cfg.Pool != nil && s.httpsish(host) {
		s.relayWithProxyReplay(c, up, host)
		return
	}
	s.relay(c, up, host)
}

// socksHandshake 处理无认证 SOCKS5 握手 + CONNECT，返回目标 host:port。
func socksHandshake(c net.Conn) (string, error) {
	// greeting: VER NMETHODS METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return "", err
	}
	if head[0] != 0x05 {
		return "", errors.New("not socks5")
	}
	nmethods := int(head[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	// 选择 no-auth (0x00)
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return "", err
	}

	// request: VER CMD RSV ATYP ...
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return "", err
	}
	if req[0] != 0x05 {
		return "", errors.New("bad socks5 ver")
	}
	if req[1] != 0x01 { // 只支持 CONNECT
		c.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) //nolint:errcheck
		return "", errors.New("only CONNECT supported")
	}
	atyp := req[3]
	var host string
	switch atyp {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	default:
		return "", errors.New("bad atyp")
	}
	// port
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(pb)
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// httpErr 向 HTTP 客户端写一个最小错误响应（net.Conn 不满足 http.ResponseWriter）。
func httpErr(c net.Conn, code int, msg string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
}

// ParseTarget 解析 CLI 传入的 host:port，缺端口按 scheme 补全。
func ParseTarget(raw string, defPort int) string {
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return raw
	}
	return net.JoinHostPort(raw, strconv.Itoa(defPort))
}

// TrimTargetHost 去掉 host:port 里的端口，只留 host（调试用）。
func TrimTargetHost(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return strings.TrimSpace(h)
}
