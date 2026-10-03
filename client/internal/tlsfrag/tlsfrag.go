// Package tlsfrag 实现 TLS-RF（TLS 分片）：把 ClientHello 拆成小片、拉开发送间隔。
//
// 原理：GFW 的 SNI 阻断依赖**重组**。TCP 本身有序，所以阻断设备可以把分片重新拼成
// 完整的 ClientHello 读出明文 SNI —— 前提是这些分片在它的重组窗口内到达。把首段切成
// 很小的片并拉开间隔，重组窗口先到期，阻断设备看到的就是一堆不完整的碎片。
//
// 两个入口，因为两个调用方拿到 ClientHello 的方式不同：
//
//   - Write：调用方**手上已经有整段**ClientHello（代理的 replay 路径会把客户端的首个
//     飞行段完整缓存下来），直接分片写出去，粒度最可控。
//   - Conn：调用方不碰 TLS 栈（节点订阅走的是标准 http.Transport，ClientHello 由
//     crypto/tls 自己写），只能包一层 net.Conn，把**前若干字节的写**打散。
//
// 参数取 var 而不是 const：测试要把间隔缩到毫秒级。
package tlsfrag

import (
	"io"
	"net"
	"sync"
	"time"
)

var (
	// Chunk 每片字节数。取小值（个位数）以保证重组窗口内凑不出一条完整记录。
	Chunk = 8

	// Delay 片间隔。要大于阻断设备的重组等待，但不必大到拖垮首包。
	Delay = 8 * time.Millisecond

	// MaxSpan 只分片前 N 字节，之后一次性写出。
	//
	// 400 覆盖了主流客户端 ClientHello 里 SNI 出现位置的整个可能范围
	// （记录头5 + 握手头4 + 版本2 + random32 + session_id + cipher_suites +
	// compression + extensions 头 ≈ 100~160 字节，SNI 是惯例上的第一个扩展）。
	// 超出这个窗口的字节与"能不能读到 SNI"无关，不值得为它付延迟。
	MaxSpan = 400
)

// Write 把 b 分片写出，返回实际写出的字节总数。用包级默认参数。
func Write(w io.Writer, b []byte) (int, error) {
	return WriteWith(w, Chunk, Delay, MaxSpan, b)
}

// WriteWith 是显式带参数的 Write。给需要独立参数的调用方（例如测试要在毫秒级跑）
// 用，避免改动包级默认值影响别的使用者。
//
// 分片只覆盖前 span 字节，其余一次性写出：再往后与识别 SNI 无关，
// 全部分片只会白白增加首包延迟。写入语义与 io.Writer 一致 —— 累计返回写入字节数，
// 出错即返回已写出的部分。
func WriteWith(w io.Writer, chunk int, delay time.Duration, span int, b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if chunk <= 0 {
		chunk = len(b)
	}
	if span > len(b) {
		span = len(b)
	}
	total := 0
	for off := 0; off < span; off += chunk {
		end := off + chunk
		if end > span {
			end = span
		}
		n, err := w.Write(b[off:end])
		total += n
		if err != nil {
			return total, err
		}
		// 最后一片后面不睡：那后面已经是一次性写出的尾巴，睡它没有意义。
		if end < len(b) {
			time.Sleep(delay)
		}
	}
	if span < len(b) {
		n, err := w.Write(b[span:])
		total += n
		return total, err
	}
	return total, nil
}

// Conn 把一条连接的前若干字节分片写出，之后原样透传。
//
// 用途是那些**不经过我们手**的 TLS ClientHello：crypto/tls 自己把它写出去，我们只能在
// 底层连接上包一层。客户端通常在最初几次 Write 里就把整个 ClientHello 送完，所以按
// "前 MaxSpan 字节"计量就够；超过的部分一律透传。
//
// 只对**写**生效，读方向不需要动 —— 阻断发生在客户端发出的方向。
type Conn struct {
	net.Conn
	mu     sync.Mutex
	budget int // 还剩多少字节需要分片
	chunk  int
	delay  time.Duration
}

// NewConn 返回一个把前 MaxSpan 字节分片写出的连接（包级默认参数）。
func NewConn(c net.Conn) *Conn {
	return NewConnWith(c, Chunk, Delay, MaxSpan)
}

// NewConnWith 是显式带参数的 NewConn。
func NewConnWith(c net.Conn, chunk int, delay time.Duration, span int) *Conn {
	return &Conn{Conn: c, budget: span, chunk: chunk, delay: delay}
}

// Fragmented 返回这个连接是否仍在分片写出（诊断/测试用）。
func (c *Conn) Fragmented() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget > 0
}

func (c *Conn) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	if c.budget <= 0 {
		c.mu.Unlock()
		return c.Conn.Write(b)
	}
	// 本次只打散预算内的部分；一次写不完就留着预算给下一次 Write。
	n := len(b)
	if n > c.budget {
		n = c.budget
	}
	c.budget -= n
	chunk, delay := c.chunk, c.delay
	c.mu.Unlock()

	written, err := WriteWith(c.Conn, chunk, delay, n, b[:n])
	if err != nil {
		return written, err
	}
	rest, err := c.Conn.Write(b[n:])
	return written + rest, err
}
