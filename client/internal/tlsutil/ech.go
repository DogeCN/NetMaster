package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// storeECHRetryConfig 把服务端下发的 RetryConfigList 写进缓存，顶掉已失效的旧配置。
//
// key 必须与 FetchECHConfigList 的默认 key（domain+"|"）一致，这样自愈后的配置
// 会被后续所有不带 dohBase 的取用方直接捡走，而不必等 TTL 过期后重新 DoH。
func storeECHRetryConfig(domain string, retry []byte) {
	if domain == "" || len(retry) == 0 {
		return
	}
	echCacheMu.Lock()
	echCache[domain+"|"] = echEntry{data: retry, expires: time.Now().Add(echTTL)}
	echCacheMu.Unlock()
}

// retryConfigFrom 判断 err 是否为"服务端拒绝了我们的 ECH 配置"；若是且服务端给了
// 新配置（RetryConfigList），返回它。这正是 ECH 协议设计的自愈通道：边缘认为你
// 手里的 ECHConfigList 过期/不认识时，会在拒绝的同时下发当前有效的配置。
func retryConfigFrom(err error) ([]byte, bool) {
	var re *utls.ECHRejectionError
	if errors.As(err, &re) && len(re.RetryConfigList) > 0 {
		return re.RetryConfigList, true
	}
	return nil, false
}

// dialPhaseTimeout 限制"TCP 已连上、但握手阶段"的等待上限。
//
// 没有它时，一次拨号的耗时上限只由 TCP 的 10s 决定，而握手阶段既没有 deadline
// 也没有超时——被 RST 注入的 IP 恰好走"TCP 通、握手挂住"这条路（实测大陆网络上
// 部分 CF IP 如此），浏览器就会看到整页加载吃满 20s+。对冲拨号把第二个节点
// 拉进来之后，这个上限就是用户能感知的首字节时间。
const dialPhaseTimeout = 6 * time.Second

// DialTLS 普通 TLS 握手（带 SNI）。
func DialTLS(addr string, port uint16, sni string, insecure bool) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(int(port))), dialPhaseTimeout)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, &tls.Config{ServerName: sni, InsecureSkipVerify: insecure, NextProtos: []string{"http/1.1"}})
	// 握手有上限：握手完成后清掉，不影响这条连接后续的长期转发
	_ = conn.SetDeadline(time.Now().Add(dialPhaseTimeout))
	if err := conn.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// ECHOverConn 在已建立的 TCP 连接 raw 上做带 ECH 的 TLS 握手。
// 允许把 TCP 直连到指定的优选 IP（而非域名），同时仍用 ECH 加密真实 SNI。
// 这解决了「优选 IP 被域名直连绕过」的问题：既能用优选 IP，又能隐 SNI。
func ECHOverConn(raw net.Conn, realSNI string, ech []byte) (net.Conn, error) {
	return ECHOverConnCfg(raw, realSNI, ech, true)
}

// ECHOverConnDeadline 与 ECHOverConnCfg 相同，但握手时限由调用方给。
//
// 为什么需要：ECHOverConnCfg 用的是"给用户建隧道"的 6 秒时限。启动探测要连着跑
// 几十个候选，沿用那个时限的话，一个挂着不回话的边缘就能把整个启动预算吃光
// （实测启动从 5.1s 涨到 13.1s 就是这么来的）。探测需要自己的、毫秒级的时限。
func ECHOverConnDeadline(raw net.Conn, realSNI string, ech []byte, insecure bool, timeout time.Duration) (net.Conn, error) {
	uconn := utls.UClient(raw, &utls.Config{
		ServerName:                     realSNI,
		InsecureSkipVerify:             insecure,
		EncryptedClientHelloConfigList: ech,
		MinVersion:                     utls.VersionTLS13,
		NextProtos:                     []string{"http/1.1"},
	}, utls.HelloGolang)
	_ = raw.SetDeadline(time.Now().Add(timeout))
	if err := uconn.Handshake(); err != nil {
		raw.Close()
		// 被拒绝说明配置过期而非 ECH 不可用：把服务端给的新配置存进缓存，
		// 让同一轮里后续的拨号直接用上。这里不重试 —— raw 是调用方的，
		// 探测循环自己会换下一个候选再试。
		if retry, ok := retryConfigFrom(err); ok {
			storeECHRetryConfig(realSNI, retry)
		} else if isStructuralECHFailure(err) {
			markECHDown()
		}
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{}) // 握手有上限；之后这条连接长期转发，不设限
	return uconn, nil
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
	_ = raw.SetDeadline(time.Now().Add(dialPhaseTimeout))
	if err := uconn.Handshake(); err != nil {
		raw.Close()
		if retry, ok := retryConfigFrom(err); ok {
			storeECHRetryConfig(realSNI, retry)
		} else if isStructuralECHFailure(err) {
			markECHDown()
		}
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{}) // 握手有上限；之后这条连接长期转发，不设限
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
	_ = raw.SetDeadline(time.Now().Add(dialPhaseTimeout))
	if err := uconn.Handshake(); err != nil {
		raw.Close()
		if retry, ok := retryConfigFrom(err); ok {
			storeECHRetryConfig(realSNI, retry)
		} else if isStructuralECHFailure(err) {
			markECHDown()
		}
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{}) // 握手有上限；之后这条连接长期转发，不设限
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
//
// 自愈：若服务端拒绝（ECHRejectionError）且附带了 RetryConfigList，说明本地缓存的
// ECHConfigList 已过期 —— 把新配置写进缓存，并用它重试一次（fresh TCP，旧连接已被
// 服务端终止）。重试仍失败才把错误交出去。这样"配置过期"从"熔断 60 秒 + 退明文"
// 变成一次额外的握手，用户无感。
func DialECH(addr string, port uint16, realSNI string, ech []byte, insecure bool) (net.Conn, error) {
	conn, err := dialECHOnce(addr, port, realSNI, ech, insecure)
	if retry, ok := retryConfigFrom(err); ok {
		storeECHRetryConfig(realSNI, retry)
		conn, err = dialECHOnce(addr, port, realSNI, retry, insecure)
	}
	return conn, err
}

func dialECHOnce(addr string, port uint16, realSNI string, ech []byte, insecure bool) (net.Conn, error) {
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
	_ = raw.SetDeadline(time.Time{}) // 握手有上限；之后这条连接长期转发，不设限
	return uconn, nil
}
