package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"netmaster/internal/entry"
	"netmaster/internal/tlsutil"
)

// Client 是连到 NetMaster Worker 的出口客户端：自有协议（见 server/src/protocol.js）
// over WebSocket over TLS，mux 复用（mux.go）。
//
// 一条传输连接的建立顺序：TCP+TLS(ECH) → WS 升级 → 发送 16 字节 auth
// （md5(PASSWORD)，鉴权在连接级一次完成，会话级不再有任何凭据）。
type Client struct {
	Node     entry.Node
	SNI      string // WS 升级与 TLS SNI 用的名字 = 服务端域名
	Auth     []byte // 16 字节，md5(utf8(PASSWORD))
	UseECH   bool
	Insecure bool
}

// echAttemptBudget 是单次拨号愿意为 ECH 付的时间上限。
//
// ECH 的作用是隐藏 SNI，属于锦上添花；一旦它卡住就不该拖累正常拨号 —— 当前网络
// 下 ECH 间歇性失败，失败时"优选 IP + 域名"两次握手各自可能一直等到 TCP 超时，
// 实测单次拨号因此在 0.3s 到 20s 之间跳。给个预算，超了就放弃 ECH 走普通 TLS，
// 并触发短路，让同一批拨号里的后续请求不再重复踩坑。
const echAttemptBudget = 2 * time.Second

// dialTLS 建立到节点的 TLS 连接。优先 ECH（隐 SNI），失败或超预算时退普通 TLS。
//
// ECH 模式：直连优选 IP（Node.Addr），而非域名。
// 原因：域名直连会让系统 DNS 解析出多个 IP，其中不少并不承载目标域名，
// SYN 超时重传把 TCP 握手拖到秒级；直连优选 IP 只要几十毫秒。
// ECH 真实 SNI 由 ECHConfig 加密，外层是 cloudflare-ech.com，不因直连 IP 而泄漏。
// 优选 IP 连不上时回退到域名直连，保证可用性。
func (c *Client) dialTLS() (net.Conn, error) {
	var echErrs []string
	if c.UseECH && tlsutil.ECHEnabled() {
		type attempt struct {
			conn net.Conn
			err  error
		}
		ch := make(chan attempt, 1)
		go func() {
			conn, err := c.dialECH()
			ch <- attempt{conn, err}
		}()

		select {
		case a := <-ch:
			if a.err == nil {
				return a.conn, nil
			}
			echErrs = append(echErrs, a.err.Error())
		case <-time.After(echAttemptBudget):
			tlsutil.MarkECHDown()
			echErrs = append(echErrs, fmt.Sprintf("exceeded %s budget", echAttemptBudget))
			// 后台那次尝试可能稍后成功返回一个连接：这里没人接手，替它关掉，
			// 否则会一直挂着直到进程结束。判空以 err 为准 —— dialECH 失败时返回
			// nil 接口，但同类代码里若换成具体指针类型就会退化成 typed-nil。
			go func() {
				if a := <-ch; a.err == nil && a.conn != nil {
					a.conn.Close()
				}
			}()
		}
	}

	// 普通 TLS 兜底。场景：ECH 不可用时（服务端返回 outer 名证书而 utls 用 outer
	// 名校验 hostname）明文 SNI 反而可用。
	conn, err := tlsutil.DialTLS(c.Node.Addr, c.Node.Port, c.SNI, c.Insecure)
	if err == nil {
		return conn, nil
	}
	// 两条路都失败时把 ECH 侧的原因一并带出去，否则排查时看不到全貌。
	if len(echErrs) > 0 {
		return nil, fmt.Errorf("tls: %w (ech also failed — %s)", err, strings.Join(echErrs, "; "))
	}
	return nil, err
}

// dialECH 走完 ECH 的完整尝试链：取 ECHConfig → 连优选 IP → 连域名。
func (c *Client) dialECH() (net.Conn, error) {
	ech, err := tlsutil.FetchECHConfigList(c.SNI, "")
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var errs []string
	if conn, err := tlsutil.DialECH(c.Node.Addr, c.Node.Port, c.SNI, ech, c.Insecure); err == nil {
		return conn, nil
	} else {
		errs = append(errs, "preferred-ip: "+err.Error())
	}
	// 回退：域名直连（系统 DNS 可能给到承载 zone 的 IP）。
	if conn, err := tlsutil.DialECH(c.SNI, c.Node.Port, c.SNI, ech, c.Insecure); err == nil {
		return conn, nil
	} else {
		errs = append(errs, "domain: "+err.Error())
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// DialWS 建立到节点的 TLS+WS 传输并完成鉴权握手。
//
// mux 路径与探测共用这一步：先把昂贵的 TCP+TLS(ECH)+WS 握手做完，mux 会话
// 在其上复用；探测只关心传输建立的耗时。
func (c *Client) DialWS() (*WSConn, error) {
	tlsConn, err := c.dialTLS()
	if err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme: "wss",
		Host:   net.JoinHostPort(c.Node.Addr, strconv.Itoa(int(c.Node.Port))),
		Path:   "/", // 协议固定路径，服务端唯一路由
	}
	header := http.Header{}
	header.Set("Host", c.SNI)
	// 关键：用 NetDialTLSContext 回传已握手的 TLS 连接，
	// 否则 gorilla 看到 wss(https) 会对已握手连接再做一次 TLS 握手而失败。
	d := websocket.Dialer{
		NetDialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return tlsConn, nil
		},
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}
	ws, _, err := d.Dial(u.String(), header)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	// 鉴权握手：16 字节 auth，一条独立消息。失败立即断，不留在半打开状态。
	if err := ws.WriteMessage(websocket.BinaryMessage, c.Auth); err != nil {
		ws.Close()
		return nil, fmt.Errorf("send auth: %w", err)
	}
	return &WSConn{ws: ws}, nil
}

// sessionOpen 构造会话开帧 payload：[port u16 BE][addrType][addr]。
// 与 server/src/protocol.js 的 parseSessionOpen 一一对应。
func sessionOpen(host string, port uint16) []byte {
	buf := make([]byte, 3, 3+len(host)+1)
	buf[0] = byte(port >> 8)
	buf[1] = byte(port)
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf[2] = 1 // ADDR_IPV4
			buf = append(buf, ip4...)
		} else {
			buf[2] = 3 // ADDR_IPV6
			buf = append(buf, ip.To16()...)
		}
	} else {
		buf[2] = 2 // ADDR_DOMAIN
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	}
	return buf
}

// WSConn 把 websocket 连接包装成 net.Conn（mux 帧以 binary 消息承载）。
type WSConn struct {
	ws   *websocket.Conn
	rbuf []byte
	rmux sync.Mutex
	wmux sync.Mutex
}

// Raw 返回底层 gorilla websocket 连接（供 mux 层直接收发帧）。
func (c *WSConn) Raw() *websocket.Conn { return c.ws }

func (c *WSConn) Read(b []byte) (int, error) {
	c.rmux.Lock()
	defer c.rmux.Unlock()
	if len(c.rbuf) == 0 {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		c.rbuf = data
	}
	n := copy(b, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *WSConn) Write(b []byte) (int, error) {
	c.wmux.Lock()
	defer c.wmux.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *WSConn) Close() error                       { return c.ws.Close() }
func (c *WSConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *WSConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *WSConn) SetDeadline(t time.Time) error      { return c.ws.SetReadDeadline(t) }
func (c *WSConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *WSConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
