// OOB 分片变体：第一个分片用 TCP 紧急数据（MSG_OOB）发出。
//
// 原理：MSG_OOB 会在发送缓冲的数据后面附带一个"紧急字节"，TCP 头里的 URG 指针随之
// 跳变。依赖按序重组读 SNI 的阻断设备对紧急指针的处理大多有缺陷——要么直接放弃
// 重组，要么把后续字节流的偏移算错——于是拼不出完整的 ClientHello。
//
// 代价与风险：紧急字节是否进入对端字节流取决于它有没有开 SO_OOBINLINE。没开
// （Linux 默认，CF 边缘即如此）时紧急字节走带外通道、不占流内位置，字节流不变；
// 开了则流里会多出一个字节，TLS 记录被破坏。因此这里沿用 SniShaper 的同款策略：
// **默认关闭**，仅在确认目标站点是"普通分片被拦、OOB 能过"时手动打开。
//
// 紧急字节取 0x00（与 SniShaper 一致）；不计入 WriteWith 的返回字节数——返回值
// 的语义是"进入字节流的字节数"，带外字节不算。
package tlsfrag

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// OOB 控制分片序列的第一片是否用 MSG_OOB 发送。默认 false，理由见文件头注释。
var OOB = false

// errOOBUnsupported 由平台实现返回：当前平台无法发 MSG_OOB（不支持的平台或
// 连接拿不到原始 fd）。调用方收到它应退回普通写，而不是把错误交给上层。
var errOOBUnsupported = errors.New("tlsfrag: MSG_OOB unsupported on this platform/connection")

// rawConnOf 取出连接的 syscall.RawConn；拿不到（类型不支持或系统调用失败）返回 nil。
func rawConnOf(c net.Conn) syscall.RawConn {
	sc, ok := c.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return nil
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil
	}
	return rc
}

// trySendOOB 尝试用 MSG_OOB 把整片 p 发出（附带一个紧急字节）。
//
// 返回 handled=true 表示"OOB 通道接管了这次写"：err 为 nil 时整片已进入字节流，
// *total 加上 len(p)；err 非 nil 时这次写失败，调用方应把错误向上传（此时部分字节
// 可能已写出，退回普通写会造成重复）。handled=false 表示当前平台/连接不支持 OOB，
// 调用方照常走普通写。
func trySendOOB(w io.Writer, p []byte, total *int) (handled bool, err error) {
	c, ok := w.(net.Conn)
	if !ok {
		return false, nil
	}
	rc := rawConnOf(c)
	if rc == nil {
		return false, nil
	}
	if err := sendOOBRaw(rc, p, 0x00); err != nil {
		if errors.Is(err, errOOBUnsupported) {
			return false, nil
		}
		return true, err
	}
	*total += len(p)
	return true, nil
}
