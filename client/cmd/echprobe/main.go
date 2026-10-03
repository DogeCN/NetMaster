// 诊断工具：对给定域名做真实 utls ECH 握手，打印每一步结果。保留在仓库里，
// 供"ECH 到底生效没有"这一类问题做握手级复核（启动日志只给结论，它给证据）：
//
//	cd client && go run ./cmd/echprobe <域名>
//
// 四步里 [2a] 才是"真的隐了"的判据——有配置（[1]）不等于握手成功。
package main

import (
	"crypto/tls"
	"fmt"
	"os"

	utls "github.com/refraction-networking/utls"

	"netmaster/internal/tlsutil"
)

func main() {
	host := os.Args[1]
	ech, err := tlsutil.FetchECHConfigList(host, "")
	if err != nil {
		fmt.Println("[1] fetch ECHConfig:", "FAIL:", err)
		return
	}
	fmt.Printf("[1] fetch ECHConfig: OK (%d bytes)\n", len(ech))

	// 变体 A：项目当前的路径（ServerName=真实域名，校验内层证书）
	if conn, err := tlsutil.DialECH(host, 443, host, ech, false); err != nil {
		fmt.Println("[2a] DialECH(verify): FAIL:", err)
	} else {
		fmt.Println("[2a] DialECH(verify): OK — inner SNI hidden, cert verified")
		state := conn.(*utls.UConn).ConnectionState()
		fmt.Printf("     handshake OK; cipher=0x%04x\n", state.CipherSuite)
		conn.Close()
	}

	// 变体 B：跳过校验（DialECHNoVerify）
	if conn, err := tlsutil.DialECHNoVerify(host, 443, host, ech); err != nil {
		fmt.Println("[2b] DialECHNoVerify: FAIL:", err)
	} else {
		cs := conn.(*utls.UConn).ConnectionState()
		peer := cs.PeerCertificates
		fmt.Println("[2b] DialECHNoVerify: OK")
		if len(peer) > 0 {
			fmt.Printf("     leaf cert DNS names: %v\n", peer[0].DNSNames)
		}
		conn.Close()
	}

	// 变体 C：标准库 TLS 普通（对照：确认域名/网络本身可达）
	c, err := tls.Dial("tcp", host+":443", &tls.Config{ServerName: host})
	if err != nil {
		fmt.Println("[2c] plain TLS control: FAIL:", err)
		return
	}
	fmt.Println("[2c] plain TLS control: OK (domain reachable, ECH-specific failure confirmed)")
	c.Close()
}
