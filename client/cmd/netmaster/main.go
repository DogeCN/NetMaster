package main

import (
	"context"
	"crypto/md5"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"netmaster/internal/config"
	"netmaster/internal/entry"
	"netmaster/internal/geoip"
	"netmaster/internal/nodepool"
	"netmaster/internal/procwait"
	"netmaster/internal/proxy"
	"netmaster/internal/route"
	"netmaster/internal/sysproxy"
)

// 连接配置的优先级：命令行 flag > config.json > 环境变量。
// 实现上把 config.json 的值当作 flag 的默认值（见 internal/config），flag 一旦
// 显式给出自然覆盖它；环境变量是最后一级兜底。
func serverDefault(cfg config.Config) string {
	if cfg.Server != "" {
		return cfg.Server
	}
	return strings.TrimSpace(os.Getenv("NETMASTER_SERVER"))
}

func passwordDefault(cfg config.Config) string {
	if cfg.Password != "" {
		return cfg.Password
	}
	if v := strings.TrimSpace(os.Getenv("NETMASTER_PASSWORD")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("PASSWORD"))
}

// normalizeServer 接受裸域名，也宽容 scheme/路径（剥掉即可）。
// 客户端自己解析域名、自己建 TLS，scheme 是内部事务。
func normalizeServer(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

// requireConn 在缺少 server/password 时给出可操作的报错，返回派生好的鉴权字节。
func requireConn(serverFlag, passwordFlag string) (string, [16]byte) {
	server := normalizeServer(serverFlag)
	if server == "" {
		fmt.Fprintln(os.Stderr, "no server: pass --server or set it in config.json / NETMASTER_SERVER")
		os.Exit(2)
	}
	if passwordFlag == "" {
		fmt.Fprintln(os.Stderr, "no password: pass --password or set it in config.json / NETMASTER_PASSWORD")
		os.Exit(2)
	}
	return server, md5.Sum([]byte(passwordFlag))
}

// resolveEntries 组装入口候选：服务端域名解析（永远可用）+ 社区优选源
// （每次启动都尝试更新，全挂退缓存）。有界等待 —— 网络全断时最多等 3 秒。
func resolveEntries(ctx context.Context, server string) ([]entry.Node, string) {
	commCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	comm, src := entry.Community(commCtx)
	nodes := entry.Merge(entry.FromServer(ctx, server), comm)
	if len(nodes) == 0 {
		fmt.Fprintln(os.Stderr, "no entries: server domain unresolvable and community sources unreachable")
		os.Exit(1)
	}
	return nodes, src
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "nodes":
		cmdNodes(os.Args[2:])
	case "restore":
		cmdRestore(os.Args[2:])
	case "watchdog":
		cmdWatchdog(os.Args[2:]) // 内部命令：由 serve 派生，不在 usage 里出现
	default:
		usage()
		// 未知子命令以非零码退出：脚本里 `netmaster serv` 这种拼错会被静默当成
		// "没做什么"，而零退出码看起来像成功。
		os.Exit(2)
	}
}

// connFlags 声明 server/password 两个 flag，默认值来自 config.json 与环境变量
// （优先级 flag > config.json > 环境变量由这里的默认值顺序保证）。
func connFlags(fs *flag.FlagSet, cfg config.Config) (server, password *string) {
	server = fs.String("server", serverDefault(cfg), "server domain (config.json: server)")
	password = fs.String("password", passwordDefault(cfg), "deployment password (config.json: password)")
	return server, password
}

// cmdNodes 持续发请求，实时打印各节点的健康度/延迟/成功率，观察自适应选路的
// 实际效果。--ipcheck 统计出口 IP 分布。
func cmdNodes(args []string) {
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fs := flag.NewFlagSet("nodes", flag.ExitOnError)
	server, password := connFlags(fs, cfg)
	target := fs.String("target", "https://www.google.com/", "probe target through proxy")
	ipcheck := fs.String("ipcheck", "", "if set, fire requests to this URL and print observed egress IPs")
	fs.Parse(args)
	host, auth := requireConn(*server, *password)

	logger := log.New(os.Stdout, "", log.Ltime)
	nodes, src := resolveEntries(context.Background(), host)
	logger.Printf("entries: %d (community: %s)", len(nodes), src)
	// 诊断命令不落盘学到的状态（StateKey 为空）：观察不该污染 serve 的亲和表。
	pool := nodepool.New(nodepool.Config{Nodes: nodes, SNI: host, Auth: auth[:]})
	logger.Println("probing entry latency...")
	pool.SortByLatency()

	router, err2 := route.LoadDefault(route.Proxy)
	if err2 != nil {
		logger.Fatal("rules: ", err2)
	}
	httpAddr := "127.0.0.1:18081"
	srv := proxy.New(proxy.Config{HTTPAddr: httpAddr, Router: router, Pool: pool, Logger: logger, DialTimeout: 15 * time.Second})
	if err := srv.Start(); err != nil {
		logger.Fatal("start: ", err)
	}
	defer srv.Close()
	logger.Printf("proxy on %s, probing %s (Ctrl+C to stop)", httpAddr, *target)

	client := &http.Client{
		Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + httpAddr)
		}},
		Timeout: 30 * time.Second,
	}

	// ipcheck 模式：统计出口 IP 分布
	if *ipcheck != "" {
		counts := map[string]int{}
		for {
			resp, err := client.Get(*ipcheck)
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			counts[strings.TrimSpace(string(b))]++
			logger.Printf("=== 出口IP分布 ===")
			type kv struct {
				ip string
				n  int
			}
			var rows []kv
			for ip, n := range counts {
				rows = append(rows, kv{ip, n})
			}
			sort.Slice(rows, func(a, b int) bool { return rows[a].n > rows[b].n })
			for _, r := range rows {
				logger.Printf("  %-40s x%d", r.ip, r.n)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	for {
		if resp, err := client.Get(*target); err == nil {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
		}
		for _, l := range pool.Stats() {
			logger.Println(l)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func cmdServe(args []string) {
	appStart := time.Now()
	cfg, cfgPath, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	server, password := connFlags(fs, cfg)
	manual := fs.Bool("manual", cfg.Manual, "don't take over the system proxy, just print listen addrs (config.json: manual)")
	rulesFile := fs.String("rules", cfg.Rules, "custom rules file (config.json: rules)")
	fs.Parse(args)
	host, auth := requireConn(*server, *password)

	logger := log.New(os.Stdout, "", log.Ltime)
	if cfgPath != "" {
		logger.Printf("config: %s", cfgPath)
	}

	// 1. 入口候选（每次启动都尝试刷新社区源；有界等待，失败退缓存）
	nodes, src := resolveEntries(context.Background(), host)
	logger.Printf("entries: %d (community: %s)", len(nodes), src)

	pool := nodepool.New(nodepool.Config{
		Nodes:    nodes,
		SNI:      host,
		Auth:     auth[:],
		UseECH:   true,
		Insecure: true,
		StateKey: host,
	})
	pool.SetDialTimeout(15 * time.Second)

	// IP 归属判断：规则未明确分流的主机靠它决定首次走直连还是代理。
	// 不阻塞启动 —— 没有表时退回"一律偏代理"的老行为，表在后台装好后生效。
	geo := geoip.New(geoip.DefaultURL, geoip.DefaultTTL)
	pool.SetGeo(geo)
	geo.Start(func(size int, fromCache bool) {
		from := "fetched"
		if fromCache {
			from = "cache"
		}
		logger.Printf("[geoip] CN ranges ready: %d (%s)", size, from)
	})

	// 先用上次的探测结果预置各节点延迟估计与主节点：启动后第一个请求就走对
	// 节点，不必等本轮探测跑完。
	if results, ok := nodepool.LoadProbeCache(nodes, nodepool.ProbeCacheTTL); ok {
		if n := pool.ApplyProbeCache(results); n > 0 {
			logger.Printf("[probe] preloaded %d/%d entry latencies from cache", n, len(nodes))
		}
	}

	// 探测节点延迟。改在后台跑：几十个节点同步探测要数秒，而这段时间代理还
	// 没起来。延迟估计的收益（首个请求走快节点）也不必在启动前就拿到 ——
	// 缓存命中时已经预置过，没有缓存时后台探测跑完前用默认顺序即可。
	if len(nodes) <= 32 {
		handle := func(results []nodepool.ProbeResult) {
			logger.Printf("[probe] %s", nodepool.Summary(results))
			if err := nodepool.SaveProbeCache(nodes, results); err != nil {
				logger.Printf("[probe] WARN save cache: %v", err)
			}
		}
		logger.Println("probing entry latency in background...")
		done := pool.ProbeAsync()
		go func() { handle(<-done) }()
	}

	// 2. 路由
	var router *route.Router
	if *rulesFile != "" {
		router, err = route.Load(*rulesFile, route.Proxy)
	} else {
		router, err = route.LoadDefault(route.Proxy)
	}
	if err != nil {
		logger.Fatal("load rules: ", err)
	}

	// 3. 监听端口自动选择：先试 8080/1080，被占则顺延。用户通常不需要知道
	// 端口号 —— 系统代理自动指向选定值；--manual 下端口只用来打印。
	httpPort, err := pickPort(8080)
	if err != nil {
		logger.Fatal("pick http port: ", err)
	}
	socksPort, err := pickPort(1080)
	if err != nil {
		logger.Fatal("pick socks port: ", err)
	}
	httpAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(httpPort))
	socksAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort))

	srv := proxy.New(proxy.Config{
		HTTPAddr:       httpAddr,
		SocksAddr:      socksAddr,
		Router:         router,
		Pool:           pool,
		DirectFallback: true,
		Logger:         logger,
		DialTimeout:    15 * time.Second,
	})
	if err := srv.Start(); err != nil {
		logger.Fatal("start proxy: ", err)
	}

	// 4. 系统代理：只在接管时设置并拉起看门狗；--manual 只打印地址。
	// 顺序是刻意的，见 docs/operations.md —— 先自愈残留，再监听、再接管。
	if cleaned, err := sysproxy.CleanupStale(); err != nil {
		logger.Printf("WARN cleanup stale system proxy: %v", err)
	} else if cleaned {
		logger.Println("[sys] cleaned up stale system proxy from a previous unclean exit")
	}

	if *manual {
		logger.Printf("manual mode. HTTP proxy: http://%s   SOCKS5: socks5://%s", httpAddr, socksAddr)
	} else {
		if _, err := sysproxy.Enable(httpAddr); err != nil {
			logger.Printf("WARN set system proxy: %v (set it manually to %s)", err, httpAddr)
			// 必须回滚：Enable 是先落盘旧值、再逐项写注册表的，中途失败意味着
			// 注册表可能已被改了一部分（比如 ProxyEnable 已置 1），而走不到还原
			// 分支，系统代理会一直悬着指向本机端口。用状态文件还原即可。
			if rerr := sysproxy.Restore(); rerr != nil {
				logger.Printf("WARN roll back system proxy: %v (run: netmaster restore)", rerr)
			} else {
				logger.Println("[sys] rolled back partial system proxy change")
			}
		} else {
			logger.Printf("[sys] system proxy ON -> %s (auto-restored on exit)", httpAddr)
			// 看门狗只保护成功设置的状态：设置失败就不留空转进程。
			spawnWatchdog(logger)
		}
	}

	logger.Printf("ready in %s. browse normally; Ctrl+C to stop and restore system proxy.",
		time.Since(appStart).Round(time.Millisecond))

	// 首次连通验证：一次真实的传输层建连（TLS+WS+auth），在后台跑，
	// 结果出来补一行日志 —— 部署是否健康，这一行就是最直接的回答。
	go func() {
		if node, err := pool.Verify(); err != nil {
			logger.Printf("隧道建立失败: %v", err)
		} else {
			logger.Printf("隧道建立成功（经节点 %s）", node)
		}
	}()

	waitForSignal()

	srv.Close()
	if !*manual {
		if err := sysproxy.Restore(); err != nil {
			logger.Printf("WARN restore system proxy: %v (run: netmaster restore)", err)
		} else {
			logger.Println("[sys] system proxy restored")
		}
	}
	logger.Println("bye")
}

// pickPort 从 preferred 开始顺延找第一个空闲端口。
// 先占住再立刻放开存在一个极小的竞态窗口（探测到 proxy.Start 之间被抢），
// 后果只是 Start 明确报端口占用，不会静默错绑。
func pickPort(preferred int) (int, error) {
	for i := 0; i < 100; i++ {
		p := preferred + i
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			ln.Close()
			return p, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return 0, err // 不是端口占用（权限/防火墙等），报真实原因
		}
	}
	return 0, fmt.Errorf("no free port from %d", preferred)
}

// spawnWatchdog 启动一个脱离的看门狗子进程，监视当前进程；当前进程异常消失时由它还原系统代理。
func spawnWatchdog(logger *log.Logger) {
	exe, err := os.Executable()
	if err != nil {
		logger.Printf("WARN locate exe for watchdog: %v", err)
		return
	}
	cmd := exec.Command(exe, "watchdog")
	cmd.Env = append(os.Environ(), "NETMASTER_OWNER_PID="+fmt.Sprint(os.Getpid()))
	// 脱离：不让看门狗继承控制台输入，独立存活。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		logger.Printf("WARN start watchdog: %v", err)
		return
	}
	// 不 Wait，让它独立运行；父进程退出后由它接管清理。
	go func() { _ = cmd.Process.Release() }()
	logger.Printf("[sys] watchdog started (pid %d) to auto-restore on crash", cmd.Process.Pid)
}

// cmdWatchdog 内部命令：等待 owner 进程退出，若系统代理仍指向 netmaster 则还原。
// serve 被强杀（任务管理器结束进程、断电）时，由它把系统代理还原回去。
//
// owner pid 走环境变量而不是命令行参数：这是两个进程之间的内部约定，用户没有理由传它，
// 而一个自称 "--owner-pid" 的 flag 会出现在 -h 里，让人以为那是个可调的东西。
func cmdWatchdog(args []string) {
	_ = args
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("NETMASTER_OWNER_PID")))
	if err != nil || pid <= 0 {
		fmt.Fprintln(os.Stderr, "watchdog 只能由 serve 派生运行（缺 NETMASTER_OWNER_PID）")
		os.Exit(2)
	}
	procwait.Wait(pid) // 阻塞到 owner 退出
	// owner 已消失：若状态文件还在，说明它没走正常清理流程 → 还原。
	if cleaned, err := sysproxy.CleanupStale(); err == nil && cleaned {
		// 看门狗静默退出即可，日志对用户不可见。
		_ = cleaned
	}
}

func cmdRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	fs.Parse(args)
	if err := sysproxy.Restore(); err != nil {
		fmt.Println("restore err:", err)
		os.Exit(1)
	}
	fmt.Println("system proxy restored (netmaster cleaned up)")
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func usage() {
	fmt.Println("usage: netmaster <serve|nodes|restore> [flags]")
	fmt.Println("  serve   - 起本地 HTTP+SOCKS5 代理并接管系统代理（日常唯一命令）")
	fmt.Println("  nodes   - 持续发请求，观察自适应选路的实时效果")
	fmt.Println("  restore - 还原系统代理（serve 被强杀后用它收拾）")
	fmt.Print("\n配置：命令行 flag > config.json > 默认值。config.json 与 serve 的 flag 一一对应：\n" +
		"  { \"server\": \"<域名>\", \"password\": \"<口令>\", \"manual\": false, \"rules\": \"\" }\n" +
		"放在当前目录或 %AppData%/netmaster/config.json。\n每个子命令加 -h 看它自己的参数。分流与出口逻辑见 docs/。\n")
}
