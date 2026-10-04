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

// FragTryer 由出口选择器实现：回答"这个域名现在值不值得先试一次分片直连"。
//
// 单独一个可选接口（而不是塞进 Exiter）：替身不实现它时行为不变，不会因为多一个
// 方法而编译不过；同时也让"这条策略"在代码里是一个可以整体关掉的东西。
type FragTryer interface {
	ShouldTryFragDirect(host string) bool
}

// DirectDownTracker 记录与查询"直连 TCP 层失败"的短记忆（5min，selector.Pool 实现）。
//
// 与 ProxyConfirmer（TLS 层被拦、代理确认后写 30min 冷却）分工见 selector.NoteDirectDown。
// 可选接口：替身不实现时退回"每次都重试直连"的老行为，不崩。
type DirectDownTracker interface {
	NoteDirectDown(host string)
	DirectDownRecently(host string) bool
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
		// 5s 而不是老默认的 15s：这个超时压在用户首屏上（拨号失败 → 502 或改道）。
		// 健康站点的 TCP 建连在 1s 内完成；15s 只服务"对死目标保持耐心"，而那正是
		// 负记忆（directDown）该管的事 —— 失败一次记 5min，比每请求等 15s 划算。
		cfg.DialTimeout = 5 * time.Second
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

// directMode 描述 dial() 建出的连接是什么形态 —— 调用方据此选择 relay 策略。
type directMode int

const (
	// exitProxy 连接经代理隧道，不是直连。
	exitProxy directMode = iota
	// directPlain 直连，首段**明文**起步。规则直连域名默认走这个：CN 站点明文
	// 大多能通，先付 400ms 分片延迟是浪费；被拦了 relayWithReplay 会补一次分片。
	directPlain
	// directFrag 直连，首段**带分片**起步。分流判 proxy 的域名走这个 —— 用户装
	// 这个软件本身就说明明文 SNI 大概率被拦，先试明文只是白等一个探测窗口。
	directFrag
)

// direct 报告这个形态是否属于直连。
func (m directMode) direct() bool { return m != exitProxy }

// dial 按路由决策建连。
//
// 返回的 directMode 里，直连形态（directPlain/directFrag）都表示"这是一次尝试性的
// 直连"：TCP 连上了，但不代表这个站真能直连（GFW 常在看到 TLS SNI 后才发 RST）。
// 调用方应据此对 HTTPS 走 relayWithReplay —— 包括规则明确要求直连的域名。
//
// 这里与旧语义的分歧值得说清楚：以前规则直连（geoip CN / 用户规则）是裸 relay，
// 理由是"那是用户自己写的规则，失败就该失败"。但那条理由只对**拨号失败**成立
// （站点真挂了，改道也救不了，现在仍然直接报错）；对"SNI 被拦"不成立 —— 用户
// 写 direct 的意思是"这个站通常该直连"，不是"被拦了也给我报错"。relayWithReplay
// 对健康站点只多付首字节缓冲的几十毫秒，对被拦域名则是一次无感改道。
func (s *Server) dial(host string) (conn net.Conn, mode directMode, err error) {
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
	if action == rules.Direct {
		// 近期这个域名直连在 TCP 层就失败过：别再付一次完整拨号超时，直接试代理。
		// 代理也不行就照常报错 —— 记忆只是省重复成本，不改变失败的结局。
		if s.cfg.DirectFallback && s.directDownRecently(host) && s.hasUsableExit() {
			if c, derr := s.cfg.Pool.Dial(host); derr == nil {
				return c, exitProxy, nil
			}
		}
		c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
		if derr != nil {
			// TCP 层失败（被拒/超时）：写下短负记忆。代理兜底在这里**不**主动做 ——
			// CONNECT 还没回 200，改道的收益只是"502 变成代理侧的 502"，而多数
			// TCP 失败（站点真挂）改道也救不了。记忆留给下一次请求省时间。
			s.noteDirectDown(host)
			return nil, exitProxy, derr
		}
		// 明文起步还是分片起步交给记忆：只有"验证过分片才行"的域名才直接带分片。
		if s.httpsish(host) && s.needsFragDirect(host) {
			return c, directFrag, nil
		}
		return c, directPlain, nil
	}
	// 先赌一把**分片直连**（TLS-RF），哪怕池子里还有可用出口。
	//
	// 为什么值得赌：分流判 proxy 只说明这个站"通常需要代理"，不代表直连一定
	// 不通 —— 而分片直连能穿过去的话就是 2 跳，绕过去是 3 跳，差的是整个
	// Worker 出站那一段（实测 CF 托管页面的抓取 3–12s，直连只要 1–2s）。
	// 用户装这个软件本身就说明普通直连不通，所以这里不问"明文行不行"，
	// 直接带分片起步（见 relayWithReplay 的 frag）。
	//
	// 记忆决定值不值得再试一次：成功记 6h（fragDirect），失败则在隧道真的送出
	// 字节之后记 30min（directBlocked，见 NoteProxyConfirmed）—— 那 30 分钟里
	// 这个域名直接走隧道，不再付探测成本。
	if s.cfg.DirectFallback && s.httpsish(host) && s.shouldTryFragDirect(host) {
		if c, derr := net.DialTimeout("tcp", host, directProbeDialTimeout); derr == nil {
			return c, directFrag, nil
		}
		// TCP 都建不起来（黑洞/无路由）：没什么可赌的，直接落隧道。
	}
	if !s.hasUsableExit() {
		if s.cfg.DirectFallback {
			// 这条分支**就是**"尝试性直连"的定义：我们没有把握它通，
			// 只是没有可用的出口了才退回来试。所以形态必须是 directFrag
			// —— 否则调用方不会走 relayWithReplay，整条"被阻断→分片→改道"
			// 的链路就一次都不会执行（这正是它此前一直惰性的原因）。
			c, derr := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
			if derr != nil {
				return nil, exitProxy, derr
			}
			return c, directFrag, nil
		}
		return nil, exitProxy, errors.New("proxy action but no usable exit")
	}
	c, derr := s.cfg.Pool.Dial(host)
	return c, exitProxy, derr
}

// shouldTryFragDirect 问出口选择器"这个域名值不值得先试分片直连"。
// 替身没实现 FragTryer 时返回 false —— 策略整体不生效，而不是崩。
func (s *Server) shouldTryFragDirect(host string) bool {
	ft, ok := s.cfg.Pool.(FragTryer)
	return ok && ft.ShouldTryFragDirect(host)
}

// needsFragDirect 查"这个域名是否已验证过分片直连可行"（含分片起步决策）。
// 替身没实现 FragDirecter 时返回 false：明文起步，被拦了再补分片。
func (s *Server) needsFragDirect(host string) bool {
	fd, ok := s.cfg.Pool.(FragDirecter)
	return ok && fd.NeedsFragDirect(host)
}

// noteDirectDown / directDownRecently 是直连 TCP 层失败短记忆的读写。
// 替身没实现 DirectDownTracker 时：不记、不查 —— 行为退回"每次都重试直连"。
func (s *Server) noteDirectDown(host string) {
	if dt, ok := s.cfg.Pool.(DirectDownTracker); ok {
		dt.NoteDirectDown(host)
	}
}

func (s *Server) directDownRecently(host string) bool {
	dt, ok := s.cfg.Pool.(DirectDownTracker)
	return ok && dt.DirectDownRecently(host)
}

// directProbeDialTimeout 是"先赌一把分片直连"那次 TCP 建连的上限。
//
// 明显短于常规 DialTimeout(15s)：这一步对**每个新域名**都要走一次，而它的性质是赌 ——
// 被黑洞的 IP（TCP SYN 石沉大海）会一直不回应，15 秒的等待直接压在用户首屏上
// （实测首请求 20.9s 就是它：15s 拨号 + 落隧道重放）。赌输了落隧道，代价可控；
// 赌赢了省掉整整一跳。
const directProbeDialTimeout = 5 * time.Second

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
// 直连（无论来自规则还是代理动作下的分片赌注）且是 HTTPS 时走 relayWithReplay ——
// 它把选择推迟到首个数据包回来之后再定，被拦时无缝改走代理；分片记忆决定首段
// 是明文还是分片起步。
// 代理路径的 HTTPS 走 relayWithProxyReplay —— 与直连侧对称：隧道已建立但不等于
// 这跳真能用（worker 内联选中继，首次可能踩到"TCP 能通但不干活"的中继），
// 上游零字节即断时换出口重放一次，浏览器无感。
func (s *Server) tunnel(client net.Conn, host string) {
	up, mode, err := s.dial(host)
	if err != nil {
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\n\r\n")

	if mode.direct() && s.httpsish(host) {
		s.relayWithReplay(client, up, host, mode == directFrag)
		return
	}
	if mode == exitProxy && s.httpsish(host) {
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
	up, mode, err := s.dial(host)
	if err != nil {
		// reply: 失败
		c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) //nolint:errcheck
		return
	}
	defer up.Close()
	// reply 成功（bound addr 填 0.0.0.0:0）
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) //nolint:errcheck
	if mode.direct() && s.httpsish(host) {
		s.relayWithReplay(c, up, host, mode == directFrag)
		return
	}
	if mode == exitProxy && s.httpsish(host) {
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
