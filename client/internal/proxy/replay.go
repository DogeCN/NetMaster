package proxy

import (
	"errors"
	"net"
	"time"
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

// relayWithReplay 处理"尝试性直连"的 CONNECT 隧道。
//
// 要解决的问题：TCP 握手成功不等于这个站能直连。GFW 常常放行 TCP，直到看见 TLS
// ClientHello 里的明文 SNI 才发 RST。而 CONNECT 一旦回了 200 就等于提交了选择，
// 浏览器随后只会报连接失败，我们却拿不到任何"该回退"的信号。
//
// 做法是把判定推迟到第一个数据包回来之后：
//  1. 收下客户端的首个飞行段（ClientHello）并转发给直连目标；
//  2. 看目标是否有任何回应 ——
//     有回应          → 直连成立，转入普通转发；
//     超时无回应      → 目标只是慢，同样认账（不误伤）；
//     零字节就被断开  → 判定直连被阻断；
//  3. 阻断时改走代理隧道，把缓存的首段数据重放进去。
//
// 浏览器全程无感，只看到握手慢了一点。
func (s *Server) relayWithReplay(client, up net.Conn, host string) {
	first := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)

	_ = client.SetReadDeadline(time.Now().Add(relayClientWait))
	n, _ := client.Read(buf)
	if n > 0 {
		first = append(first, buf[:n]...)
		// 短窗口续读：把同一批到达的分片凑齐。
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
		// 客户端还没发数据（超时或已断开）→ 没有可重放的内容，退回普通转发。
		s.relay(client, up, host)
		return
	}
	if _, err := up.Write(first); err != nil {
		return
	}

	probe := make([]byte, 32*1024)
	_ = up.SetReadDeadline(time.Now().Add(relayProbeWait))
	pn, perr := up.Read(probe)
	_ = up.SetReadDeadline(time.Time{})

	if pn > 0 {
		// 直连成立。先把探到的数据送给客户端，再转入普通转发。
		if _, err := client.Write(probe[:pn]); err != nil {
			return
		}
		s.relay(client, up, host)
		return
	}
	if isTimeoutErr(perr) {
		// 目标只是慢：认账，不据此改道。
		s.relay(client, up, host)
		return
	}

	// 零字节即断开 → 直连被判阻断，改走代理并重放首段。
	//
	// 注意措辞：能确定的只是"直连没能把这段数据送出去"，原因可能是 RST、连接被
	// 关闭，也可能是**上游 worker 侧的出站连接失败**（比如目标是 Cloudflare 承载的，
	// 而 Worker 不能连 CF 自己的 IP），不能一律归为 "blocked by RST" —— 会把后者
	// 也误报成防火墙阻断，排查时被带偏。
	reason := "peer closed before responding"
	if perr != nil {
		reason = perr.Error()
	}
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
	s.cfg.Logger.Printf("[route] %s direct unusable (%s) — switched to proxy and replayed", host, reason)
	s.relay(client, pc, host)
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
