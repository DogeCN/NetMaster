// fragprobe —— 诊断工具：对同一个目标 IP，用不同的首段形态做真实 TLS 握手，
// 看哪一种能穿过去。保留在仓库里，理由与 echprobe 相同：把"能不能过"这件事
// 从猜测变成可复现的实验。
//
// 用法：
//
//	cd client && go run ./cmd/fragprobe <域名> <IP>[:端口] [每形态重复次数]
//
// 例：
//
//	go run ./cmd/fragprobe www.bbc.com 151.101.1.91 3
//
// 它只回答一个问题：**同一台机器、同一个目标 IP，首段怎么发才能拿到 ServerHello**。
// 明文（不分组）与项目默认参数（8B/8ms/前400B）都列在里面，方便对照。
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"netmaster/internal/tlsfrag"
)

type shape struct {
	name  string
	chunk int
	delay time.Duration
	span  int
	oob   bool // 第一片走 MSG_OOB（tlsfrag.OOB）
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: fragprobe <domain> <ip[:port]> [repeats]")
		os.Exit(2)
	}
	domain := os.Args[1]
	addr := os.Args[2]
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	repeats := 3
	if len(os.Args) > 3 {
		if n, err := strconv.Atoi(os.Args[3]); err == nil && n > 0 {
			repeats = n
		}
	}

	shapes := []shape{
		{"不分组（对照）", 1 << 20, 0, 0, false},
		{"旧默认 8B/8ms/400B", 8, 8 * time.Millisecond, 400, false},
		{"现默认（包级 Chunk/Delay/MaxSpan）", tlsfrag.Chunk, tlsfrag.Delay, tlsfrag.MaxSpan, false},
		{"1B/0/400B（400 小段，不等待）", 1, 0, 400, false},
		{"1B/1ms/400B", 1, time.Millisecond, 400, false},
		{"1B/0/1B（只切第一字节）", 1, 0, 1, false},
		{"1B/0/100B", 1, 0, 100, false},
		{"5B/0/5B（切一刀就走）", 5, 0, 5, false},
		{"2B/0/400B", 2, 0, 400, false},
		{"16B/16ms/400B", 16, 16 * time.Millisecond, 400, false},
		// OOB 变体：第一片用 TCP 紧急数据发出（见 tlsfrag/oob.go 的原理）。
		// 它们要回答的是"紧急指针能不能让按序重组的阻断设备算错偏移"。
		{"OOB+1B/0/1B", 1, 0, 1, true},
		{"OOB+5B/0/5B", 5, 0, 5, true},
		{"OOB+8B/8ms/400B", 8, 8 * time.Millisecond, 400, true},
		{"OOB+1B/1ms/400B", 1, time.Millisecond, 400, true},
		// 边界扫描：钉住"片多大开始失效"。1B/2B 过、5B 不过，中间那一档决定默认值。
		{"3B/0/400B", 3, 0, 400, false},
		{"4B/0/400B", 4, 0, 400, false},
		{"6B/0/400B", 6, 0, 400, false},
		{"1B/0/50B", 1, 0, 50, false},
		{"1B/0/200B", 1, 0, 200, false},
	}

	fmt.Printf("target %s (%s), repeats=%d\n", domain, addr, repeats)
	fmt.Printf("%-34s %-8s %s\n", "首段形态", "成功率", "耗时（成功的那几次）")
	for _, s := range shapes {
		ok, times, lastErr := run(domain, addr, s, repeats)
		timing := "-"
		if len(times) > 0 {
			parts := make([]string, 0, len(times))
			for _, d := range times {
				parts = append(parts, d.Round(time.Millisecond).String())
			}
			timing = strings.Join(parts, " ")
		}
		note := ""
		if ok == 0 && lastErr != "" {
			note = "  ← " + lastErr
		}
		fmt.Printf("%-34s %-8s %s%s\n", s.name, fmt.Sprintf("%d/%d", ok, repeats), timing, note)
	}
}

func run(domain, addr string, s shape, repeats int) (ok int, times []time.Duration, lastErr string) {
	for i := 0; i < repeats; i++ {
		d, err := once(domain, addr, s)
		if err == nil {
			ok++
			times = append(times, d)
			continue
		}
		lastErr = shortErr(err)
	}
	return ok, times, lastErr
}

// once 做一次"TCP 连上 → 按指定形态发 ClientHello → 等 ServerHello"。
//
// 证书不校验（InsecureSkipVerify）：这里要回答的是"阻断设备放不放行"，
// 不是"证书对不对"；校验失败会把一个成功的穿透误报成失败。
func once(domain, addr string, s shape) (time.Duration, error) {
	start := time.Now()
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(8 * time.Second))

	conn := tlsfrag.NewConnWith(raw, s.chunk, s.delay, s.span)
	// OOB 是包级开关（与客户端 config.json 的 frag-oob 同源），探针里逐个形态设置。
	// 串行执行，所以不需要同步；跑完恢复成关闭，避免影响后续形态。
	tlsfrag.OOB = s.oob
	defer func() { tlsfrag.OOB = false }()
	tc := tls.Client(conn, &tls.Config{
		ServerName:         domain,
		InsecureSkipVerify: true, //nolint:gosec // 见 once 的说明
		MinVersion:         tls.VersionTLS12,
	})
	if err := tc.Handshake(); err != nil {
		return 0, err
	}
	_ = tc.Close()
	return time.Since(start), nil
}

func shortErr(err error) string {
	m := err.Error()
	if len(m) > 60 {
		m = m[:60]
	}
	return m
}
