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
//	① 直连首段    t=0   明文或分片起步（见 fragStart），等 relayProbeWait(3s)
//	② 分片重试    仅在①明文起步且被拦时   重新拨一次 + 约 400ms 分片
//	③ 代理隧道    仅在②也失败时          沿用已有 mux
//
// 起步形态由调用方按域名性质决定：分流判 proxy 的域名**带分片起步**（用户装这个
// 软件本身就说明明文 SNI 早被 RST，先试明文只是白等一个探测窗口）；规则直连的
// 域名**明文起步**（CN 站点明文大多能通，先付分片延迟是浪费），被拦了自然落到②。
//
// ②/③排在后面是这里的核心：留在直连就**不多绕一跳**、不在服务端多烧一次
// connect() 子请求（50 子请求/invocation 是免费版硬顶）。这正是 TLS-RF 的意义
// —— 穿过去，而不是绕过去。
//
// 分片成功之后记住这个域名（selector.Pool，6h），下次直接带分片起步。
//
// 浏览器全程无感，只看到握手慢了一点。
func (s *Server) relayWithReplay(client, up net.Conn, host string, fragStart bool) {
	// 本请求如果在 dial() 里认领了探测权，在这里统一释放 —— 无论从哪个出口
	// 离开（直连成立 / 阶梯②成立 / 落代理 / 客户端断开）。未认领时是 no-op。
	defer s.endProbe(host)

	endTunnel := s.cfg.Trace.Begin("connect."+routeTag(host), 0)
	defer endTunnel()

	first := s.spoolFirstFlight(client)
	if first == nil {
		// 客户端还没发数据 → 没有可重放的内容，退回普通转发。
		s.relay(client, up, host)
		return
	}

	fd := s.fragDirecter()
	// 分片记忆可以把明文起步升级成分片起步：规则直连域名若上一轮验证过"必须
	// 分片"，就没必要每次都先吃一次拦截再补。remembered 只用于日志与打点。
	remembered := fd != nil && fd.NeedsFragDirect(host)
	frag := fragStart || remembered
	endWrite := s.cfg.Trace.Begin("replay.write-first", 0,
		fmt.Sprintf("fragmented=%v remembered=%v", frag, remembered))
	writeErr := writeFlight(up, first, frag)
	endWrite()

	// 首段写不出去（对端已经关了）时**不能提前 return**：浏览器那侧已经收到
	// "200 Connection Established"，提前返回等于把它挂在一个死隧道上。正确做法是
	// 当成"这条路不通"，继续往下走代理兜底 —— 这与"零字节即断"是同一类结论。
	//
	// 这条以前没暴露，是因为明文首段是一次性写出去的：写到已关闭的 socket 上
	// 本地缓冲多半会成功，于是照样走到探测那一步；而分片是多次写，中途就失败了。
	reason := ""
	if writeErr == nil {
		data, perr := probeUp(up, relayProbeWait)
		if data != nil {
			// 直连成立。先记住"这个域名分片直连可行"（6h），再把探到的数据送给
			// 客户端，然后转入普通转发。
			//
			// 记忆必须在这里写：以前只有"明文被拦 → 分片重试成功"那条路会写它，
			// 而分片优先之后第一次尝试就带分片，那条路不再经过 —— 漏了这里，
			// 每条新连接都会把分片当"第一次赌"，日志里也永远学不到结论。
			if fd != nil {
				fd.NoteFragDirect(host)
			}
			if _, err := client.Write(data); err != nil {
				return
			}
			s.cfg.Trace.Mark("replay.direct-ok", fmt.Sprintf("probe=%s", relayProbeWait))
			s.relay(client, up, host)
			return
		}
		if isTimeoutErr(perr) {
			// 超时**按阻断处理**，不再当"目标只是慢"。
			//
			// 这是 2026-10-04 的有意反转。旧契约是"超时不是被阻断的证据"，理由是
			// 别把慢但活的站点推去代理。但这条路径现在对**每个新域名**都要走一次
			// （分片直连优先），而实测的阻断形态里"TCP 连上、ClientHello 被静默丢弃"
			// 比 RST 更常见（BBC/Wikipedia 明文直连都是 20s 无响应而非 RST）。
			// 把它当"慢"的后果是：请求留在一条永远不会回应的直连上，用户干等自己的
			// 超时（实测 25s 以上）。
			//
			// 代价：首字节真的超过 relayProbeWait 的目标会被推去代理，并记 30 分钟。
			// 取舍依据：被墙网络里"静默丢弃"远多于"慢站点"，而多绕一跳远好过整页挂死。
			reason = fmt.Sprintf("no response within %s (silent drop, treated as blocked)", relayProbeWait)
			s.cfg.Trace.Mark("replay.silent-drop", "")
		} else {
			// 注意措辞：能确定的只是"直连没能把这段数据送出去"，原因可能是 RST、连接被
			// 关闭，也可能是**上游 worker 侧的出站连接失败**（比如目标是 Cloudflare 承载的，
			// 而 Worker 不能连 CF 自己的 IP），不能一律归为 "blocked by RST" —— 会把后者
			// 也误报成防火墙阻断，排查时被带偏。
			reason = "peer closed before responding"
			if perr != nil {
				reason = perr.Error()
			}
		}
	} else {
		reason = writeErr.Error()
	}

	// ② 明文起步被拦：还差一步分片没试。重新拨一次直连，带分片把首段重写。
	// 成功就留在直连（2 跳），并把"这个域名要分片"记下来；失败才落③。
	if !frag {
		if up2, data2, ok := s.retryWithFrag(host, first); ok {
			_ = up.Close()
			if _, err := client.Write(data2); err != nil {
				return
			}
			s.relay(client, up2, host)
			return
		}
		// 重拨失败时 up 还是那条已建立的直连 —— 它TCP 是通的，别白白扔掉：
		// 浏览器那侧 200 已经回出去了，宁可让它干等到自己的超时。
		s.cfg.Trace.Mark("replay.frag-retry-unavailable", "")
	}

	// 分片也没穿过去 ⇒ 摘掉"分片可行"这条记忆（如果有）。
	//
	// 这个分支以前只在"因为记忆才分片"时走，于是记忆清不掉：fragDirect 的优先级
	// 高于 directBlocked（见 selector.Pool 的字段注释），而下面紧接着写下的
	// "这个域名该走代理"会被它压掉。后果是此后 6 小时内每条连接都先白付约 400ms
	// 的分片、再落代理 —— 一次失败的探测，代价按 TTL 持续计费。
	if fd != nil {
		fd.ForgetFragDirect(host)
		if remembered {
			s.cfg.Logger.Printf("[frag] %s remembered fragmentation no longer works (%s) — forgetting it", host, reason)
		}
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

// retryWithFrag 是 relayWithReplay 的阶梯②：对明文起步被拦的域名，重新拨一条
// 直连、带分片重写首段并探测。返回 (conn, data, true) 表示分片直连成立 —— data 是
// 探测窗口里收到的目标首字节，调用方必须先把它回给客户端再转普通 relay；
// (nil, nil, false) 表示这条路不通（调用方维持原状或落代理）。
//
// 判定语义与①一致：拿到数据 = 通过；超时/断开 = 不通。
func (s *Server) retryWithFrag(host string, first []byte) (net.Conn, []byte, bool) {
	up2, err := net.DialTimeout("tcp", host, s.cfg.DialTimeout)
	if err != nil {
		s.cfg.Trace.Mark("replay.frag-retry-dial-fail", err.Error())
		return nil, nil, false
	}
	if werr := writeFlight(up2, first, true); werr != nil {
		up2.Close()
		s.cfg.Trace.Mark("replay.frag-retry-write-fail", werr.Error())
		return nil, nil, false
	}
	data, perr := probeUp(up2, relayProbeWait)
	if data == nil {
		reason := "no response (silent drop, treated as blocked)"
		if perr != nil && !isTimeoutErr(perr) {
			reason = perr.Error()
		}
		up2.Close()
		s.cfg.Trace.Mark("replay.frag-retry-blocked", reason)
		return nil, nil, false
	}
	if fd := s.fragDirecter(); fd != nil {
		fd.NoteFragDirect(host)
	}
	s.cfg.Logger.Printf("[frag] %s plain direct was blocked — TLS fragmentation got through, staying direct", host)
	s.cfg.Trace.Mark("replay.frag-retry-ok", "")
	return up2, data, true
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
	up2, mode, derr := s.dial(host)
	if derr != nil {
		s.cfg.Logger.Printf("[route] %s proxy tunnel dead (%s), retry failed: %v", host, reason, derr)
		return
	}
	defer up2.Close()
	// 重拨可能认领了探测权（分片赌注），但这条路径把连接直接拿去用、不进
	// relayWithReplay —— 不释放的话这个域名的赌注会被永久关掉。
	if mode.direct() {
		s.endProbe(host)
	}
	if _, err := up2.Write(first); err != nil {
		return
	}
	s.cfg.Logger.Printf("[route] %s proxy tunnel dead (%s) — switched exit and replayed", host, reason)
	s.relay(client, up2, host)
}
