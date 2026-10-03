package proxy

import (
	"errors"
	"fmt"
	"net"
	"time"

	"netmaster/internal/tlsfrag"
)

const (
	// relayClientWait 等客户端发出第一段数据（TLS 场景即 ClientHello）的上限。
	relayClientWait = 10 * time.Second
	// relayFlushWait 是"把同一批分片收齐"的续读窗口。客户端在收到 ServerHello
	// 之前不会继续发，所以第一段是自包含的，给个小窗口即可读全。
	relayFlushWait = 30 * time.Millisecond
	// relayProbeWait 是等目标回第一个字节的上限。
	//
	// 超时**不算**被阻断 —— 目标只是慢而已，据此改道会误伤正常站点。
	// 只有"一个字节都没回就被断开"才判定为阻断（GFW 的 SNI 阻断就是这种形态）。
	relayProbeWait = 3 * time.Second
	// relayProxyProbeWait 是**代理隧道**专用的首字节窗口。
	//
	// 比直连侧宽得多，因为这条路要多付一跳中继：KV 里实测的健康中继延迟就有
	// 1~2.6s，再加目标自身的首字节，3 秒窗口会把"慢但活"的隧道判成死的 ——
	// 代价是丢掉已经建立的隧道、再付一次拨号（实测表现为资源密集页面整页超时）。
	// 真死的隧道几乎都是立刻 EOF（RST/close），不等窗口到期，所以放宽窗口
	// 并不牺牲故障切换的速度。
	relayProxyProbeWait = 8 * time.Second
	// relayFragProbeWait 是**分片直连**的首字节窗口。
	//
	// 比明文侧略宽，因为分片本身已经付掉了 MaxSpan/Chunk × Delay 的延迟
	// （默认约 400ms）才开始等回应；而目标在看到分片 ClientHello 后才决定
	// 收不收，给它一点余量免得把"碎片拼得慢"误判成被墙。
	relayFragProbeWait = 4 * time.Second
	// relaySpoolMax 是首个飞行段的缓存上限，超出就不再收（正常 ClientHello 约 200~2000 字节）。
	relaySpoolMax = 16 * 1024
)

// isHTTPSPort 判断 CONNECT 目标是否是 HTTPS（443）。
//
// 只对 443 启用探测重放：HTTPS 首段（ClientHello）自包含、且 GFW 的 SNI 阻断
// 正发生在这里；其他协议可能"客户端先发一半再等回应"，探测窗口会白白多等 3 秒。
func isHTTPSPort(host string) bool {
	_, port, err := net.SplitHostPort(host)
	return err == nil && port == "443"
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// routeTag 给 span 名字里的目标做一次收敛。
//
// 直接用 host 会让一次跑出几百个 span 名，报告立刻失去可读性；而排障真正需要的
// 是"哪一类目标慢"。域名不唯一但**端口**通常能区分路径（443 走首段探测与分片，
// 其他端口走普通转发），所以按端口分桶。
func routeTag(host string) string {
	if _, port, err := net.SplitHostPort(host); err == nil {
		return port
	}
	return "other"
}

// FragDirecter 是"这个域名要不要把 TLS 首段分片"的记忆层。selector.Pool 实现。
//
// 为什么是可选接口而不是 Exiter 的方法：记忆层不是出口选择的前提，缺了它
// 只是少一条优化路径，主流程仍然工作（那就每次都重新学一遍）。
type FragDirecter interface {
	// NeedsFragDirect 查该域名是否已验证过"分片直连可行"。
	NeedsFragDirect(host string) bool
	// NoteFragDirect 分片直连真的通了 —— 记住它，下次省掉注定失败的明文尝试。
	NoteFragDirect(host string)
	// ForgetFragDirect 分片这次没奏效 —— 忘掉，否则过期记忆会持续白付分片延迟。
	ForgetFragDirect(host string)
}

// ProxyConfirmer 在代理隧道真的送出字节之后被调用（实现 selector.Pool.NoteProxyConfirmed）。
type ProxyConfirmer interface {
	NoteProxyConfirmed(host string)
}

// spoolFirstFlight 收下客户端的首个飞行段（TLS 场景即 ClientHello）。
//
// 收齐而不是只收一次：浏览器往往把 ClientHello 拆成几个 TCP 段发过来，只读一次
// 会拿到半截 TLS 记录。客户端在收到 ServerHello 之前不会继续发，所以第一段是
// 自包含的，给个小窗口续读即可。
//
// 返回 nil 表示客户端还没发出任何数据（超时或已断开）—— 没有可重放的内容。
func (s *Server) spoolFirstFlight(client net.Conn) []byte {
	first := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)

	_ = client.SetReadDeadline(time.Now().Add(relayClientWait))
	n, _ := client.Read(buf)
	if n > 0 {
		first = append(first, buf[:n]...)
		_ = client.SetReadDeadline(time.Now().Add(relayFlushWait))
		for len(first) < relaySpoolMax {
			m, merr := client.Read(buf)
			if m > 0 {
				first = append(first, buf[:m]...)
			}
			if merr != nil {
				break
			}
		}
	}
	_ = client.SetReadDeadline(time.Time{})

	if n == 0 {
		return nil
	}
	return first
}

// probeUp 等 up 回第一个字节，连同数据一起返回。
//
// 返回 (data 非空, nil) 直连成立；(nil, timeout) 目标只是慢；
// (nil, 其它) 一个字节都没回就被断开 —— 那是 GFW 的 SNI 阻断形态。
// **只有最后一种**才据此改道。
func probeUp(up net.Conn, wait time.Duration) ([]byte, error) {
	probe := make([]byte, 32*1024)
	_ = up.SetReadDeadline(time.Now().Add(wait))
	n, err := up.Read(probe)
	_ = up.SetReadDeadline(time.Time{})
	if n <= 0 {
		return nil, err
	}
	return probe[:n], nil
}

// writeFlight 把首个飞行段写出去；fragmented 为真时按 TLS-RF 分片。
func writeFlight(up net.Conn, first []byte, fragmented bool) error {
	if fragmented {
		_, err := tlsfrag.Write(up, first)
		return err
	}
	_, err := up.Write(first)
	return err
}

func (s *Server) fragDirecter() FragDirecter {
	fd, _ := s.cfg.Pool.(FragDirecter)
	return fd
}

func (s *Server) noteProxyConfirmed(host string) {
	if pc, ok := s.cfg.Pool.(ProxyConfirmer); ok {
		pc.NoteProxyConfirmed(host)
	}
}

// relayWithReplay 处理"尝试性直连"的 CONNECT 隧道。
//
// 要解决的问题：TCP 握手成功不等于这个站能直连。GFW 常常放行 TCP，直到看见 TLS
// ClientHello 里的明文 SNI 才发 RST。而 CONNECT 一旦回了 200 就等于提交了选择，
// 浏览器随后只会报连接失败，我们却拿不到任何"该回退"的信号。
//
// 做法是把判定推迟到第一个数据包回来之后，并且**按代价从低到高**依次尝试：
//
//	① 明文直连   t=0     等 relayProbeWait(3s)
//	② 分片直连   仅在①被判阻断时   重新拨一次 + 约 400ms 分片
//	③ 代理隧道   仅在②也失败时     沿用已有 mux
//
// ②排在③之前是这里的核心：分片成功就**留在直连**，不多绕一跳、也不在服务端多烧
// 一次 connect() 子请求（50 子请求/invocation 是免费版硬顶）。这正是 TLS-RF 的意义
// —— 穿过去，而不是绕过去。
//
// ②成功之后记住这个域名（selector.Pool，6h），下次直接跳过注定失败的①。
//
// 浏览器全程无感，只看到握手慢了一点。
func (s *Server) relayWithReplay(client, up net.Conn, host string) {
	endTunnel := s.cfg.Trace.Begin("connect."+routeTag(host), 0)
	defer endTunnel()

	first := s.spoolFirstFlight(client)
	if first == nil {
		// 客户端还没发数据 → 没有可重放的内容，退回普通转发。
		s.relay(client, up, host)
		return
	}

	fd := s.fragDirecter()
	frag := fd != nil && fd.NeedsFragDirect(host)
	endWrite := s.cfg.Trace.Begin("replay.write-first", 0, fmt.Sprintf("fragmented=%v", frag))
	if err := writeFlight(up, first, frag); err != nil {
		endWrite()
		return
	}
	endWrite()

	data, perr := probeUp(up, relayProbeWait)
	if data != nil {
		// 直连成立。先把探到的数据送给客户端，再转入普通转发。
		if _, err := client.Write(data); err != nil {
			return
		}
		s.cfg.Trace.Mark("replay.direct-ok", fmt.Sprintf("probe=%s", relayProbeWait))
		s.relay(client, up, host)
		return
	}
	if isTimeoutErr(perr) {
		// 目标只是慢：认账，不据此改道。
		s.cfg.Trace.Mark("replay.slow-kept-direct", "")
		s.relay(client, up, host)
		return
	}

	// 注意措辞：能确定的只是"直连没能把这段数据送出去"，原因可能是 RST、连接被
	// 关闭，也可能是**上游 worker 侧的出站连接失败**（比如目标是 Cloudflare 承载的，
	// 而 Worker 不能连 CF 自己的 IP），不能一律归为 "blocked by RST" —— 会把后者
	// 也误报成防火墙阻断，排查时被带偏。
	reason := "peer closed before responding"
	if perr != nil {
		reason = perr.Error()
	}

	// ② 分片直连。明文都被拦了才轮到它，所以这里不该再付一次"明文试错"的时间。
	if !frag {
		if s.tryFragmentedDirect(client, up, first, host, reason) {
			return
		}
	} else if fd != nil {
		// 我们**是因为**这条记忆才分片的，而分片这次没奏效 ⇒ 摘掉它。
		//
		// 这个分支以前是空的，于是记忆永远清不掉：fragDirect 的优先级高于
		// directBlocked（见 selector.Pool 的字段注释），而下面紧接着写下的
		// "这个域名该走代理"会被它压掉。后果是此后 6 小时内每条连接都先白付约
		// 400ms 的分片、再落代理 —— 一次失败的探测，代价按 TTL 持续计费。
		fd.ForgetFragDirect(host)
		s.cfg.Logger.Printf("[frag] %s remembered fragmentation no longer works (%s) — forgetting it", host, reason)
	}

	// ③ 代理隧道。分片也穿不过去，说明这条路对我们不成立，只能绕。
	pc, derr := s.retryViaProxy(host)
	if derr != nil {
		s.cfg.Logger.Printf("[route] %s direct unusable (%s), proxy retry failed: %v", host, reason, derr)
		return
	}
	defer pc.Close()
	_ = up.Close()
	if _, err := pc.Write(first); err != nil {
		return
	}
	s.noteProxyConfirmed(host)
	s.cfg.Logger.Printf("[route] %s direct unusable (%s) — switched to proxy and replayed", host, reason)
	s.relay(client, pc, host)
}

// tryFragmentedDirect 用分片后的 ClientHello 重新走一次直连。
//
// 返回 true 表示本函数已经接管（成功转发，或明确失败并把 up 关掉），调用方不要
// 再往下走代理；返回 false 表示这条路不可用，调用方应当继续走代理兜底。
//
// 为什么必须**重新拨**一条连接：触发它的那次直连已经被对端 RST 掉了，
// 同一�� socket 上重发只会得到同样的 RST。
func (s *Server) tryFragmentedDirect(client, dead net.Conn, first []byte, host, reason string) bool {
	endT := s.cfg.Trace.Begin("frag.retry", 0)
	defer endT()

	fd := s.fragDirecter()
	if fd == nil {
		// 没有记忆层就永远学不会分片可行 —— 但**仍然试一次**是值得的：
		// 这次试的成本是一次拨号加约 400ms，而成功的话这次调用就把它记住了。
		fd = &statelessFragDirecter{}
	}
	_ = dead.Close()

	endDial := s.cfg.Trace.Begin("frag.redial", 0)
	up2, err := s.dialDirect(host)
	endDial()
	if err != nil {
		s.cfg.Logger.Printf("[frag] %s fragmented direct could not redial: %v", host, err)
		return false
	}
	endFrag := s.cfg.Trace.Begin("frag.write", 0, fmt.Sprintf("%d bytes", len(first)))
	_, err = tlsfrag.Write(up2, first)
	endFrag()
	if err != nil {
		_ = up2.Close()
		return false
	}
	data, _ := probeUp(up2, relayFragProbeWait)
	if data == nil {
		_ = up2.Close()
		fd.ForgetFragDirect(host)
		s.cfg.Logger.Printf("[frag] %s fragmented direct also blocked (%s) — falling back to proxy", host, reason)
		return false
	}

	fd.NoteFragDirect(host)
	// 探到的首包（TLS 场景即 ServerHello）必须先交给客户端，否则它会一直等
	// 一个已经被我们读走的回应，表现为握手挂死。
	if _, err := client.Write(data); err != nil {
		_ = up2.Close()
		return true
	}
	s.cfg.Logger.Printf("[frag] %s direct was blocked (%s) — TLS fragmentation got through, staying direct", host, reason)
	s.relay(client, up2, host)
	return true
}

// statelessFragDirecter 是"没有记忆层"时的空实现：让 tryFragmentedDirect 仍然
// 能跑一次并给出正确的成败，只是不留下跨连接的结论。
type statelessFragDirecter struct{}

func (statelessFragDirecter) NeedsFragDirect(string) bool { return false }
func (statelessFragDirecter) NoteFragDirect(string)       {}
func (statelessFragDirecter) ForgetFragDirect(string)     {}

// dialDirect 拨一条直连。
//
// 绕过 Router 与 Pool 是**刻意的**：能走到这里说明分流已经判给代理、而出口全灭，
// 我们是在"没有选择"的处境里赌一把直连。此时再问一次规则没有意义（结果只会是
// 同样的代理出口），而 Pool.Dial 只会把我们送回那个已经证明不通的路径。
//
// 测试可注入：Config.DirectDial。
func (s *Server) dialDirect(host string) (net.Conn, error) {
	if s.cfg.DirectDial != nil {
		return s.cfg.DirectDial(host)
	}
	return net.DialTimeout("tcp", host, s.cfg.DialTimeout)
}

// retryViaProxy 在被判定阻断后改用代理重连。
func (s *Server) retryViaProxy(host string) (net.Conn, error) {
	if s.cfg.RetryViaProxy != nil {
		return s.cfg.RetryViaProxy(host)
	}
	if s.cfg.Pool == nil {
		return nil, errors.New("proxy: no node pool for retry")
	}
	return s.cfg.Pool.RetryProxy(host)
}

// relayWithProxyReplay 处理代理路径的 CONNECT 隧道 —— 与 relayWithReplay 同一
// 哲学的另一半：直连侧的问题是"TCP 连通 ≠ 没被墙"，代理侧的问题是"隧道建立
// ≠ 这跳真能用"。worker 首次见到一个 Cloudflare 承载的目标时要内联选中继，
// 可能先踩到"TCP 能通但不干活"的中继；此时 200 已经回了，TLS 握手期隧道死亡，
// 浏览器只能看到一个莫名的安全错误。
//
// 做法与直连侧对称：缓存首个飞行段并转发，若上游在回任何字节之前死掉，
// 解绑该域名的出口、给绑定节点记一次失败，换一个出口重放一次。只重试一次，
// 且只对"零字节即断"负责 —— 已经有数据回来的会话属于中途断流，重放解决不了
// （浏览器看到的半截 TLS 记录无法撤销），好在 worker 侧会把坏中继忘掉，
// 下一次连接自然命中别的中继。
//
// 探测窗口语义与直连侧不同：mux 会话的 SetReadDeadline 到期即关闭会话，
// 所以"超时无回应"在这里也按死亡处理 —— 443 的 ServerHello 来自边缘，
// 正常情况远快于 3 秒，值得为之重试而不是让浏览器干等一条可能已死的隧道。
func (s *Server) relayWithProxyReplay(client, up net.Conn, host string) {
	first := s.spoolFirstFlight(client)
	if first == nil {
		s.relay(client, up, host)
		return
	}
	if _, err := up.Write(first); err != nil {
		return
	}

	data, _ := probeUp(up, relayProxyProbeWait)
	if data != nil {
		if _, err := client.Write(data); err != nil {
			return
		}
		s.relay(client, up, host)
		return
	}

	reason := "peer closed before responding"
	// 这次隧道没用：解绑出口并记一次失败，换一个出口重放。
	if s.cfg.Pool != nil {
		s.cfg.Pool.NoteProxyFailure(host)
	}
	up2, _, derr := s.dial(host)
	if derr != nil {
		s.cfg.Logger.Printf("[route] %s proxy tunnel dead (%s), retry failed: %v", host, reason, derr)
		return
	}
	defer up2.Close()
	if _, err := up2.Write(first); err != nil {
		return
	}
	s.cfg.Logger.Printf("[route] %s proxy tunnel dead (%s) — switched exit and replayed", host, reason)
	s.relay(client, up2, host)
}
