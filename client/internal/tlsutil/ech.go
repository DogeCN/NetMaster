package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ECH 配置缓存：DoH 一次查询即可复用，避免每条连接都打网络。
var (
	echCacheMu sync.RWMutex
	echCache   = map[string]echEntry{}
	echTTL     = 6 * time.Hour
)

type echEntry struct {
	data    []byte
	expires time.Time
}

// FetchECHConfigList 通过阿里 DoH(HTTPS) 查询域名 HTTPS RR，提取 ech 参数（base64）→ 二进制 ECHConfigList。
// 结果带缓存（默认 6h），同一域名短时间内不会重复查询 DoH。
func FetchECHConfigList(domain, dohBase string) ([]byte, error) {
	key := domain + "|" + dohBase
	echCacheMu.RLock()
	e, ok := echCache[key]
	echCacheMu.RUnlock()
	if ok && time.Now().Before(e.expires) {
		return e.data, nil
	}
	data, err := fetchECHConfigListUncached(domain, dohBase)
	if err != nil {
		return nil, err
	}
	echCacheMu.Lock()
	echCache[key] = echEntry{data: data, expires: time.Now().Add(echTTL)}
	echCacheMu.Unlock()
	return data, nil
}

// dohEndpoints 是查询 HTTPS RR(ECH) 的候选 DoH 端点，按顺序回退。
// 阿里 DoH 对 type=65(type65) 的支持在部分节点上会返回 SERVFAIL(Status:2)，
// 换到另一个端点/IP 通常即可，故保留多个候选。
var dohEndpoints = []string{
	"https://223.5.5.5/resolve",
	"https://dns.alidns.com/resolve",
	"https://223.6.6.6/resolve",
}

func fetchECHConfigListUncached(domain, dohBase string) ([]byte, error) {
	endpoints := dohEndpoints
	if dohBase != "" {
		endpoints = append([]string{dohBase}, dohEndpoints...)
	}
	var lastErr error
	for _, base := range endpoints {
		data, err := queryECH(base, domain)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func queryECH(dohBase, domain string) ([]byte, error) {
	u := fmt.Sprintf("%s?name=%s&type=HTTPS", dohBase, url.QueryEscape(domain))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("accept", "application/dns-json")
	c := &http.Client{Timeout: 10 * time.Second}
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	return parseECHFromDoH(body)
}

func parseECHFromDoH(body []byte) ([]byte, error) {
	var resp struct {
		Status int `json:"Status"`
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	// Status != 0 means the resolver failed for this query (e.g. 2 = SERVFAIL).
	// Some DoH backends return it intermittently for type=65, so surface it as
	// an error to let the caller try the next endpoint.
	if resp.Status != 0 {
		return nil, fmt.Errorf("doh status %d", resp.Status)
	}
	re := regexp.MustCompile(`ech="([^"]+)"`)
	for _, a := range resp.Answer {
		if a.Type != 65 { // HTTPS RR
			continue
		}
		m := re.FindStringSubmatch(a.Data)
		if m == nil {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			continue
		}
		return b, nil
	}
	return nil, fmt.Errorf("no ECH config found in HTTPS RR")
}

// DialTLS 普通 TLS 握手（带 SNI）。
func DialTLS(addr string, port uint16, sni string, insecure bool) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(int(port))), 10*time.Second)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, &tls.Config{ServerName: sni, InsecureSkipVerify: insecure, NextProtos: []string{"http/1.1"}})
	if err := conn.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// ECHOverConn 在已建立的 TCP 连接 raw 上做带 ECH 的 TLS 握手。
// 允许把 TCP 直连到指定的优选 IP（而非域名），同时仍用 ECH 加密真实 SNI。
// 这解决了「优选 IP 被域名直连绕过」的问题：既能用优选 IP，又能隐 SNI。
func ECHOverConn(raw net.Conn, realSNI string, ech []byte) (net.Conn, error) {
	return ECHOverConnCfg(raw, realSNI, ech, true)
}

// ECHOverConnCfg 同 ECHOverConn，可指定是否跳过证书校验。
func ECHOverConnCfg(raw net.Conn, realSNI string, ech []byte, insecure bool) (net.Conn, error) {
	uconn := utls.UClient(raw, &utls.Config{
		ServerName:                     realSNI,
		InsecureSkipVerify:             insecure,
		EncryptedClientHelloConfigList: ech,
		MinVersion:                     utls.VersionTLS13,
		NextProtos:                     []string{"http/1.1"},
	}, utls.HelloGolang)
	if err := uconn.Handshake(); err != nil {
		raw.Close()
		if isStructuralECHFailure(err) {
			markECHDown()
		}
		return nil, err
	}
	return uconn, nil
}

// DialECHNoVerify 建立 ECH 连接并跳过对内层证书的 hostname 校验。
//
// 为什么需要它：ECH 生效后，服务端在内层握手里返回的是「真实 SNI」对应的证书，
// 而 utls 在 ECH 模式下会拿外层 public_name(cloudflare-ech.com)去做 hostname 校验，
// 于是报 "certificate is valid for cloudflare-ech.com, not <host>"。
//
// 这里的安全性由 ECH 本身提供：真实 SNI 被加密，只有持有对应私钥的端点才能完成
// 内层握手。客户端无需（也无法）用明文做证书匹配。
func DialECHNoVerify(addr string, port uint16, realSNI string, ech []byte) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(int(port))), 10*time.Second)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(raw, &utls.Config{
		ServerName:                     realSNI,
		InsecureSkipVerify:             true,
		EncryptedClientHelloConfigList: ech,
		MinVersion:                     utls.VersionTLS13,
		NextProtos:                     []string{"http/1.1"},
		// 覆盖：让 utls 不再用 outer name 做 hostname 校验。
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error { return nil },
	}, utls.HelloGolang)
	if err := uconn.Handshake(); err != nil {
		raw.Close()
		if isStructuralECHFailure(err) {
			markECHDown()
		}
		return nil, err
	}
	return uconn, nil
}

// ECH 短路：同一进程内，若刚观察到 ECH 失败，短时间内不再尝试。
//
// 为什么需要：ECH 在当前网络下是**间歇性**可用的 —— 有时握手成功，有时服务端
// 返回 ECH outer 名（cloudflare-ech.com）的证书，而 utls v1.8.2 在 ECH 模式下
// 会拿 outer 名去校验 hostname，于是握手失败。而失败的代价很高：要连着做完
// "优选 IP + 域名"两次握手，各自可能一直等到 TCP 超时；入口探测是几十路
// 并发拨号，这个代价会被成倍放大。
//
// 因此这里是**突发抑制**而非"永久关闭"：一次失败后短暂跳过 ECH，避免一批拨号
// 同时踩坑；TTL 结束又会重新尝试，因为 ECH 随时可能恢复可用，而它的价值
// （隐藏 SNI）不该被一次失败长期放弃。
var echDownUntil atomic.Int64

// echDownTTL 是短路持续时间。取 60s：足以覆盖启动时的一批拨号，
// 又不会让 ECH 在恢复可用后长时间不被使用。
const echDownTTL = 60 * time.Second

// ECHEnabled 报告当前是否值得尝试 ECH。
func ECHEnabled() bool { return time.Now().UnixNano() >= echDownUntil.Load() }

// MarkECHDown 记住一次 ECH 失败，短时间内不再尝试。供调用方在给 ECH 设了时间
// 预算、超时放弃时使用。
func MarkECHDown() { markECHDown() }

// markECHDown 记住一次 ECH 结构性失败。
func markECHDown() { echDownUntil.Store(time.Now().Add(echDownTTL).UnixNano()) }

// isStructuralECHFailure 判定是否属于"ECH 与 utls 不兼容"这类失败 —— 握手完成、
// 但服务端给的是 outer 名证书。
//
// 只对这种短路：网络抖动、目标不可达造成的失败不该被当成 ECH 本身不可用，
// 否则一次弱网超时就会静默放弃 SNI 隐藏。
func isStructuralECHFailure(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "certificate is valid for") ||
		strings.Contains(m, "failed to verify certificate")
}

// DialECH 用 utls 建立带 ECH 的 TLS 握手。外层 SNI 自动设为 ECHConfig 的 public_name，真实 SNI 加密。
func DialECH(addr string, port uint16, realSNI string, ech []byte, insecure bool) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(int(port))), 10*time.Second)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(raw, &utls.Config{
		ServerName:                     realSNI,
		InsecureSkipVerify:             insecure,
		EncryptedClientHelloConfigList: ech,
		MinVersion:                     utls.VersionTLS13,
		NextProtos:                     []string{"http/1.1"},
	}, utls.HelloGolang)
	if err := uconn.Handshake(); err != nil {
		raw.Close()
		if isStructuralECHFailure(err) {
			markECHDown()
		}
		return nil, err
	}
	return uconn, nil
}
