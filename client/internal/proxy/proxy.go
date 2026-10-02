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

	"netmaster/internal/route"
)

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
	Router *route.Router
	// Pool 出口选择器（直连/代理决策与建连）。
	Pool Exiter
	// DirectFallback 路由未覆盖或节点池为空时是否直连（默认 true）
	DirectFallback bool
	// RetryViaProxy 在"尝试性直连"被判定阻断后改用代理重连（见 relayWithReplay）。
	// 默认走 Pool.RetryProxy；测试可注入替身。
	RetryViaProxy func(host string) (net.Conn, error)
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

// dial 按路由决策建连。
//
// 返回的 tentativeDirect 表示"这是一次尝试性的直连"：TCP 连上了，但不代表这个站
// 真能直连（GFW 常在看到 TLS SNI 后才发 RST）。调用方应据此决定要不要走
// relayWithReplay。规则明确要求直连的（比如局域网、.cn 名单）不算尝试性。
func (s *Server) dial(host string) (conn net.Conn, tentativeDirect bool, err error) {
	action := route.Proxy
	if s.cfg.Router != nil {
		action = s.cfg.Router.Decide(host)
	}
	switch action {
	case route.Direct:
		c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
		return c, false, derr
	case route.Auto:
		// 未明确分流：交给出口层按 IP 归属 + 域名亲和选，失败互相回退。
		if s.cfg.Pool == nil || s.cfg.Pool.Len() == 0 {
			c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
			return c, false, derr
		}
		return s.cfg.Pool.DialAuto(host)
	default: // proxy
		if s.cfg.Pool == nil || s.cfg.Pool.Len() == 0 {
			if s.cfg.DirectFallback {
				c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
				return c, false, derr
			}
			return nil, false, errors.New("proxy action but no node pool")
		}
		c, derr := s.cfg.Pool.Dial(host)
		return c, false, derr
	}
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
	if req.URL.Port() == "" {
		// 原样保留 scheme 默认端口
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))
		if req.URL.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))
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

	if tentativeDirect && isHTTPSPort(host) {
		s.relayWithReplay(client, up, host)
		return
	}
	if !tentativeDirect && s.cfg.Pool != nil && isHTTPSPort(host) {
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
	if tentativeDirect && isHTTPSPort(host) {
		s.relayWithReplay(c, up, host)
		return
	}
	if !tentativeDirect && s.cfg.Pool != nil && isHTTPSPort(host) {
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
