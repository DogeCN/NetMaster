package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"netmaster/internal/config"
	"netmaster/internal/entry"
	"netmaster/internal/geoip"
	"netmaster/internal/instlock"
	"netmaster/internal/procwait"
	"netmaster/internal/profile"
	"netmaster/internal/proxy"
	"netmaster/internal/rules"
	"netmaster/internal/selector"
	"netmaster/internal/sysproxy"
	"netmaster/internal/tlsfrag"
)

// boolWord 把布尔旋钮写成"开/关"而不是 "true/false"：指纹是给人读的，
// "ech=enabled" 比 "ech=true" 少一次翻译。
func boolWord(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

// rulesFileLabel 把"用了哪份自定义规则"写进指纹。空与"某文件"是两种不同的
// 分流行为，代价也不同，必须能区分。
func rulesFileLabel(path string) string {
	if path == "" {
		return "(builtin)"
	}
	return path
}

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

// tunnelDefault 返回 -tunnels 的缺省值：配置里**写了**就用它，否则用 selector
// 的内置默认。
//
// 这里必须判 nil 而不是判值：`{"tunnels": 0}` 和 `{"tunnels": -3}` 都是写了，
// 且都是无效配置，应该被后面的校验报错退出。之前写成 `n > 0` 时它们会静默变成
// 4 条隧道 —— 用户以为自己配了 0（等于"别开隧道"），实际跑的是 4 条。
//
// 补齐到真实数字还有另一个理由：命令行帮助里要印出缺省值，用户才知道
// 自己填的 2 是"少一半"而不是"随便一个数"。
func tunnelDefault(cfg config.Config) int {
	if cfg.Tunnels != nil {
		return *cfg.Tunnels
	}
	return selector.DefaultMuxTarget
}

// recheckRounds 是 NETMASTER_PROBE_DEBUG 下每个被拒候选串行重探几轮。
const recheckRounds = 3

// recheckDelay 读 NETMASTER_PROBE_RECHECK_DELAY（秒），用来把重探推迟到限流窗口
// 之后 —— 这是区分"本来就坏"与"我们自己打的"唯一靠得住的办法。
func recheckDelay() time.Duration {
	v := os.Getenv("NETMASTER_PROBE_RECHECK_DELAY")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
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

// doubleClick 是"无参数启动"的标记：双击 exe 时没有任何参数，此时出错的话
// 控制台窗口会随进程一起消失，错误没人看得见 —— 所以退出前等一次回车；
// 终端里带参数启动则保持即时退出，不拖累脚本。
var doubleClick bool

// fatal 打印错误并退出；双击启动时等待回车，让用户看清窗口里的原因。
func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	if doubleClick {
		fmt.Fprintln(os.Stderr, "\npress Enter to close...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(1)
}

// requireConn 在缺少 server/password 时给出可操作的报错。
// v2 鉴权用原始口令在首帧做 HMAC-SHA256（见 internal/proto），口令不出网络。
func requireConn(serverFlag, passwordFlag string) (string, string) {
	server := normalizeServer(serverFlag)
	if server == "" {
		fatal("no server: pass --server or set it in config.json / NETMASTER_SERVER")
	}
	if passwordFlag == "" {
		fatal("no password: pass --password or set it in config.json / NETMASTER_PASSWORD")
	}
	return server, passwordFlag
}

// rulesActionFromFile 按文件名后缀判定整份列表的动作。第二个返回值是"认出来了没有"。
//
// 抽成纯函数是为了能测：rulesOverridesFromFile 自己会 fatal 退出，测不了。
func rulesActionFromFile(path string) (rules.Action, bool) {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".direct"):
		return rules.Direct, true
	case strings.HasSuffix(lower, ".proxy"):
		return rules.Proxy, true
	}
	return "", false
}

// rulesOverridesFromFile 把 --rules 指向的本地规则文件转成"自定义源"。
// 空路径返回空 Overrides = 用内置源。
//
// 格式与 Clash RULE-SET 一致，动作由**文件名后缀**显式给出：
// `xxx.direct` → 整份列表直连，`xxx.proxy` → 整份列表代理。
//
// 推断不出就报错退出，不再"猜一个默认值"：先前的实现按文件名里含不含
// "direct" 来判断，`myrules.list` 这种正常名字会被静默当成 proxy，而用户
// 完全看不出自己的文件被当成什么处理了。看得见的失败比看不见的降级好。
func rulesOverridesFromFile(path string) rules.Overrides {
	path = strings.TrimSpace(path)
	if path == "" {
		return rules.Overrides{}
	}
	action, ok := rulesActionFromFile(path)
	if !ok {
		fatal("--rules file must end with .direct or .proxy (got: " + path + ")\n" +
			"  name it like 'mylist.direct' so every entry in it has a known action")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		// 用户给了坏路径不该静默按内置源跑：看得见的失败比看不见的降级好
		fatal("read rules file: " + err.Error())
	}
	parsed, _ := rules.ParseClashRuleset("custom", string(body), action)
	if len(parsed) == 0 {
		fatal("rules file " + path + " has no usable entries")
	}
	// 自定义源优先级最高：直接构造 Router，内置源不再拉取。
	return rules.Overrides{Custom: parsed}
}

// resolveEntries 组装入口候选：服务端域名解析（永远可用）+ 社区优选源
// （每次启动都尝试更新，全挂退缓存）。有界等待 —— 网络全断时最多等 3 秒。
//
// maxEntries 是入口候选的总上限。DNS 源排在最前所以必然保留；社区源超过
// 部分直接截断。
//
// 64 而不是"优选后保留的 16"：这两个数字管的是不同阶段。Optimize 之后只有
// 16 个进节点池，但如果候选一开始就只有 16 个，那么探测时一旦发现几个不可达
// 或延迟异常（单源抖动很常见），池子就被填不满。多备几倍是给探测留冗余 ——
// 12 并发探测 64 个仍在秒级完成，代价可接受。
const maxEntries = 64

// dnsHeadroom 是服务端域名解析结果先占的名额。DNS 源排在最前且"永远可用"
// （社区源挂了它还得兜底），但也不能让它吃掉整个预算——实测它通常只回 1~4 个 A 记录，
// 给 16 个名额足够，宽了就是浪费社区源的位置。
const dnsHeadroom = 16

func resolveEntries(ctx context.Context, server string) ([]entry.Node, string) {
	commCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// 两类源并行（PRD §6.5 step 2）：服务端域名解析与社区优选互不依赖，
	// 串行的话慢的那类直接拖慢启动。
	var (
		wg      sync.WaitGroup
		lists   [][]entry.Node
		src     string
		fromSrv []entry.Node
	)
	wg.Add(2)
	go func() { defer wg.Done(); lists, src = entry.CommunityLists(commCtx) }()
	go func() { defer wg.Done(); fromSrv = entry.FromServer(commCtx, server) }()
	wg.Wait()

	// 名额按源轮转交错分配，不按到达顺序盲截：社区源响应时间差异大（协助者实测
	// 522 / 896 / 1395ms）且单源条目数可以超过上限本身（pages.dev 150 > 64），
	// 盲截会让"每次启动活下来的那批"随机变化 —— 节点池不可复现，任何前后对比都
	// 失去意义。DNS 源排在最前且必须全收，单独先占一段名额。
	head := fromSrv
	if len(head) > dnsHeadroom {
		head = head[:dnsHeadroom]
	}
	nodes := append(head, entry.Interleave(lists, maxEntries-len(head))...)
	if len(nodes) == 0 {
		fatal("no entries: server domain unresolvable and community sources unreachable")
	}
	return nodes, src
}

// reportProbe 把一次探测的结果写进启动日志。
//
// ECH 的成败必须说在启动日志里：它的失败是**静默**的 —— 握手失败被判成结构性故障、
// 把 ECH 熔断 60 秒，之后每次拨号都退回明文 SNI，用户照样能上网却不知道自己以为被
// 隐着的服务端域名一直明文在外面。实测这个事实藏了整整一个版本，所以这里明说。
//
// 两段耗时也分别印出来：启动慢的时候，"哪一段慢"和"慢多少"才是能据此动手的信息，
// 一句"探测用了 10 秒"只会让人干瞪眼。
func reportProbe(logger *log.Logger, tr *profile.Trace, host string, insecure bool, total int, pr selector.OptResult) {
	switch {
	case !pr.ECHConfigured:
		logger.Printf("[ech] no ECH config published for this domain - connections will use a visible SNI")
	case !pr.ECHWorked:
		logger.Printf("[ech] ECH handshake failed on every attempt - connections fall back to a VISIBLE SNI " +
			"(see docs: ECH must be enabled on the Cloudflare zone; until then the server domain is in the clear)")
	default:
		logger.Printf("[ech] SNI hidden by ECH")
	}
	logger.Printf("[probe] %d entries -> %d nodes in %s (tcp %s, tls %s, ws upgrade %s, %d entry IPs refused)",
		total, len(pr.Nodes), pr.ProbeTook.Round(time.Millisecond),
		pr.ScreenTook.Round(time.Millisecond), pr.TLSTook.Round(time.Millisecond),
		pr.UpgradeTook.Round(time.Millisecond), pr.Rejected)
	if tr != nil {
		tr.FactInt("nodes", len(pr.Nodes))
		tr.FactInt("refused", pr.Rejected)
	}
	// 被拒的入口明细只在显式要求时打：正常启动的用户不需要知道这些，
	// 而排障的人需要能整段贴出来（"这些 IP 为什么不行"没有别的来源）。
	if os.Getenv("NETMASTER_PROBE_DEBUG") == "" {
		return
	}
	for _, e := range pr.TLSErrors {
		logger.Printf("[probe] tls failed %s", e)
	}
	for _, r := range pr.Refused {
		logger.Printf("[probe] refused %s:%d — %s", r.Node.Addr, r.Node.Port, r.Err)
	}
	// 把被拒的串行重探一遍，分成"真的建不了隧道"和"被我们自己的并发打出来的"。
	// 没有这个拆分的话，被拒条数是个被污染的上界，拿它算"该不该做升级验证"
	// 的账一定会算错 —— 详见 selector.RecheckRefused 的说明。
	if len(pr.Refused) == 0 {
		return
	}
	// 重探前先等一会儿（可选）：要区分"这个 IP 本来就建不了隧道"与
	// "Worker 那边的限流窗口还没过去"，必须等窗口过去再问。
	if d := recheckDelay(); d > 0 {
		logger.Printf("[probe] waiting %s before the serial re-probe", d)
		time.Sleep(d)
	}
	rc := selector.RecheckRefused(context.Background(), pr.Refused, host, insecure, recheckRounds)
	logger.Printf("[probe] recheck: %d still refused, %d were our own concurrency's fault (serial re-probe, %d rounds each)",
		len(rc.StillRefused), len(rc.FalsePos), rc.Rounds)
	for _, r := range rc.StillRefused {
		logger.Printf("[probe] really refused %s:%d — %s", r.Node.Addr, r.Node.Port, r.Err)
	}
	for _, r := range rc.FalsePos {
		logger.Printf("[probe] false positive %s:%d (refused in the concurrent pass, fine alone)", r.Node.Addr, r.Node.Port)
	}
}

func main() {
	if len(os.Args) < 2 {
		// 无参数 = 双击启动：直接 serve，用法说明走 -h / help。
		doubleClick = true
		cmdServe(nil)
		return
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		usage()
		return
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

// connFlags 声明 server/password 两个 flag。server 的默认值来自 config.json 与
// 环境变量；password 刻意注册成空默认 —— flag 包会把默认值打进 -h 输出，部署
// 口令不该躺在任何人的终端回滚缓冲里。真实值在解析后由 passwordValue 给出
// （优先级不变：flag > config.json > 环境变量）。
func connFlags(fs *flag.FlagSet, cfg config.Config) (server, password *string) {
	server = fs.String("server", serverDefault(cfg), "server domain (config.json: server)")
	password = fs.String("password", "", "deployment password (config.json: password; value hidden from -h)")
	return server, password
}

// passwordValue 解析口令的真实值。
func passwordValue(flagVal *string, cfg config.Config) string {
	if flagVal != nil && *flagVal != "" {
		return *flagVal
	}
	return passwordDefault(cfg)
}

// cmdNodes 持续通过本地代理发请求，实时看这条链路能不能通、通得多快。
//
// 名字里的 "nodes" 是历史遗留（它曾经逐节点打印延迟/成功率/健康度）。v2 里
// 节点池在启动时就优选好了，运行时也不再逐节点打分 —— 现在它老实做一件事：
// 反复请求 --target 并把结果打出来。想看出口 IP 分布用 --ipcheck。
func cmdNodes(args []string) {
	cfg, _, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fs := flag.NewFlagSet("nodes", flag.ExitOnError)
	server, password := connFlags(fs, cfg)
	target := fs.String("target", "https://www.google.com/", "URL to fetch through the proxy, repeatedly")
	ipcheck := fs.String("ipcheck", "", "if set, fetch this URL and print the observed egress IP distribution")
	fs.Parse(args)
	host, pw := requireConn(*server, passwordValue(password, cfg))

	logger := log.New(os.Stdout, "", log.Ltime)
	nodes, src := resolveEntries(context.Background(), host)
	logger.Printf("entries: %d (community: %s)", len(nodes), src)
	pool := selector.New(selector.Config{Nodes: nodes, SNI: host, Password: pw, UseECH: !cfg.ECHDisabled(), Insecure: cfg.InsecureEnabled()})

	router, rerr := rules.BuiltinOnly(nil, nil)
	if rerr != nil {
		logger.Fatal("rules: ", rerr)
	}
	logger.Printf("rules: %d (%s)", router.Size(), router.Source())
	httpAddr := "127.0.0.1:18081"
	srv := proxy.New(proxy.Config{HTTPAddr: httpAddr, Router: router, Pool: pool, Logger: logger, DialTimeout: 15 * time.Second})
	if err := srv.Start(); err != nil {
		logger.Fatal("start: ", err)
	}
	defer srv.Close()
	logger.Printf("proxy on %s, fetching %s every 2s (Ctrl+C to stop)", httpAddr, *target)

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
			logger.Printf("=== egress IP distribution ===")
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
		resp, err := client.Get(*target)
		if err != nil {
			logger.Printf("request failed: %v", err)
		} else {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			logger.Printf("ok %s", resp.Status)
		}
		time.Sleep(2 * time.Second)
	}
}

func cmdServe(args []string) {
	appStart := time.Now()

	// 采集器要在最早期就决定，因为启动阶段恰好包含最值得量的几段。
	// 未开启时 tr 是 nil，全部调用都是空操作，热路径上不留任何 if。
	profOn, profPath := profile.FromEnv()
	tr := profile.New(profOn)

	cfg, cfgPath, err := config.Load()
	if err != nil {
		fatal(err.Error())
	}
	if doubleClick && cfgPath == "" {
		// 首次双击且没有任何配置：生成模板再退出，用户填好两个值即可再点。
		path, _ := config.WriteTemplate()
		fatal("no config.json found — created a template at " + path +
			"\nopen it, fill in server and password, then start netmaster again")
	}

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	server, password := connFlags(fs, cfg)
	manual := fs.Bool("manual", cfg.Manual, "don't take over the system proxy, just print listen addrs (config.json: manual)")
	rulesFile := fs.String("rules", cfg.Rules, "custom rules file (config.json: rules)")
	noECH := fs.Bool("no-ech", cfg.ECHDisabled(), "disable ECH SNI hiding on the client->worker hop (config.json: no-ech)")
	fragOOB := fs.Bool("frag-oob", cfg.FragOOBValue(), "send the first ClientHello fragment as TCP urgent data (MSG_OOB); only for sites where plain fragmentation is blocked (config.json: frag-oob)")
	tunnels := fs.Int("tunnels", tunnelDefault(cfg), "simultaneous proxy tunnels, 1-8; more helps busy pages but burns the free DO time quota (config.json: tunnels)")
	fs.Parse(args)
	host, pw := requireConn(*server, passwordValue(password, cfg))
	tlsfrag.OOB = *fragOOB
	if *tunnels < 1 || *tunnels > selector.MaxMuxTarget {
		fatal(fmt.Sprintf("tunnels must be between 1 and %d, got %d", selector.MaxMuxTarget, *tunnels))
	}

	// 指纹事实：凡是会改变耗时的旋钮都要登记，否则两次运行的数字不可比，
	// 而不可比的数字比没有数字更坏（见 profile 包的说明）。
	if tr != nil {
		tr.Fact("tunnels", strconv.Itoa(*tunnels))
		tr.Fact("ech", boolWord(*noECH, "disabled", "enabled"))
		tr.Fact("insecure", boolWord(cfg.InsecureEnabled(), "on", "off"))
		tr.Fact("rules-file", rulesFileLabel(*rulesFile))
		tr.Fact("frag", fmt.Sprintf("%dB/%dms/%dB%s", tlsfrag.Chunk, tlsfrag.Delay.Milliseconds(), tlsfrag.MaxSpan, boolWord(*fragOOB, "+oob", "")))
	}

	// 单实例（PRD §6.5 step 1）：两个 serve 会互相抢系统代理。锁只拦"接管系统
	// 代理"的实例——--manual 不碰系统代理，允许多开（诊断/并行观察是正当需求）。
	// 锁文件系统失败只降级告警，不拦着用户上网。
	var unlock func()
	var lockWarn error
	if !*manual {
		var holder int
		var lerr error
		unlock, holder, lerr = instlock.Acquire(config.LockPath(cfgPath))
		switch {
		case errors.Is(lerr, instlock.ErrLocked):
			// 命名互斥体模式下拿不到持有者的真实 pid；锁文件里的 pid 可能是过期
			// 记录，只作参考。
			fatal(fmt.Sprintf("another netmaster serve is already running (last recorded pid %d).\nstop that instance first; if the system proxy is stuck, run: netmaster restore", holder))
		case lerr != nil:
			lockWarn = lerr
		}
	}

	logger := log.New(os.Stdout, "", log.Ltime)
	if tr != nil {
		defer func() {
			// 报告直接走 stdout 而不是 logger：logger 每行带时间戳，而这份报告
			// 自己就有偏移列，再叠一层时间戳只会让人分不清哪列是哪列。
			_ = tr.WriteReport(os.Stdout)
			if err := tr.WriteJSON(profPath); err != nil {
				logger.Printf("WARN profile json: %v", err)
			}
		}()
	}
	if lockWarn != nil {
		logger.Printf("WARN single-instance lock: %v (continuing without it)", lockWarn)
	} else if unlock != nil {
		defer unlock()
	}
	if cfgPath != "" {
		logger.Printf("config: %s", cfgPath)
	}

	// 1. 入口候选分两批到。服务端域名的 DNS 解析是毫秒级的，社区优选列表要一两秒
	//    （实测每源 0.5–1.4s，整体 3s 预算）。启动路径**只等前者**：探测、建隧道、
	//    就绪都不该被社区源拖住 —— 用户在这段时间里什么都做不了。后者到了在后台
	//    探测、并进池子（selector.Pool.AddNodes）。
	entriesStart := time.Now()
	dnsNodes := entry.FromServer(context.Background(), host)
	if len(dnsNodes) > dnsHeadroom {
		dnsNodes = dnsNodes[:dnsHeadroom]
	}
	communityPending := true
	if len(dnsNodes) == 0 {
		// 域名一个 IP 都解析不出来：退回"等社区源"的老路径。空池子比慢启动更糟 ——
		// Dial 在没有候选时直接失败，用户看到的是整页打不开。
		logger.Printf("entries: server domain did not resolve; waiting for community sources")
		dnsNodes, _ = resolveEntries(context.Background(), host)
		communityPending = false
	}
	tr.Mark("entries.dns", fmt.Sprintf("%d candidates from server DNS", len(dnsNodes)))
	if communityPending {
		logger.Printf("entries: %d (server DNS; community still pending)", len(dnsNodes))
	} else {
		logger.Printf("entries: %d (community, server DNS empty)", len(dnsNodes))
	}
	// 节点池的**实际成员**是耗时的一部分：探测耗的是被选中的那批，不同的成员
	// 意味着不同的网络路径。不登记它，"这次快了 2 秒"就可能是"这次池子更好"。
	if tr != nil {
		tr.FactInt("entries", len(dnsNodes))
		tr.FactInt("entry-fetch-ms", int(time.Since(entriesStart).Milliseconds()))
	}

	// IP 归属判断：规则未明确分流的主机靠它决定首次走直连还是代理。
	// 不阻塞启动 —— 没有表时退回"一律偏代理"的老行为，表在后台装好后生效。
	// 先于探测构造：规则加载要用它（geoip 规则），而规则加载要与探测并行。
	geo := geoip.New(geoip.DefaultURL, geoip.DefaultTTL)
	geo.Start(func(size int, fromCache bool) {
		from := "fetched"
		if fromCache {
			from = "cache"
		}
		logger.Printf("[geoip] CN ranges ready: %d (%s)", size, from)
	})

	// 2. 规则加载（只用内置集：不拉订阅、不读磁盘缓存，见 rules.BuiltinOnly 的理由）。
	//    它仍在关键路径上：分流规则要在监听开始时就位。原来与它并行的探测已经挪到
	//    就绪之后的后台（见下面的 probeDNS）。
	endRules := tr.Begin("rules.load", 500*time.Millisecond)
	rulesOverrides := rulesOverridesFromFile(*rulesFile)
	router, rerr := rules.BuiltinOnly(rulesOverrides.Custom, geo)
	if rerr != nil {
		endRules()
		fatal("load rules: " + rerr.Error())
	}
	endRules()
	logger.Printf("rules: %d entries (%s, skipped %d lines)", router.Size(), router.Source(), router.SkippedLines())

	// 3. 节点池：先用服务端 DNS 那几个候选把隧道立起来，**不等探测**。
	//    探测的产物（按延迟排序、剔掉升级被拒的）对这几个"自己域名的 A 记录"意义有限，
	//    而它要花一秒上下；坏候选由池子自己的失败记忆处理（noteFailure → 判死 → 换一个）。
	pool := selector.New(selector.Config{
		Nodes:    dnsNodes,
		SNI:      host,
		Password: pw,
		UseECH:   !*noECH,
		// 缺省校验证书（config.json 可设 "insecure": true 关闭）。标准部署下
		// SNI 就是 server 域名，边缘返回该域名的正规证书，校验应当通过；
		// 校验失败属于真实攻击面，宁可连不上让用户看见，也不静默放行。
		Insecure:  cfg.InsecureEnabled(),
		MuxTarget: *tunnels,
	})
	logger.Printf("tunnels: %d simultaneous connections to the edge", pool.MuxTarget())

	// 3a. 社区优选：后台拉取 → 探测 → 并进池子。
	//
	// 失败只是"没有额外候选"：可用性由服务端 DNS 那条路保证（它不依赖任何第三方）。
	// 这也是为什么它可以放到后台 —— 社区源的价值是"多几个 IP 抗封锁"，不是"能不能用"。
	if communityPending {
		go func() {
			commCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			lists, src := entry.CommunityLists(commCtx)
			limit := maxEntries - pool.Len()
			if limit < 0 {
				limit = 0
			}
			extra := entry.Interleave(lists, limit)
			if len(extra) == 0 {
				logger.Printf("entries: community gave nothing (%s); staying on server DNS", src)
				return
			}
			// 直接进池，**不等探测**。
			//
			// 探测（TCP 筛查 + TLS + WS 升级）与"拨一条 mux"验的是同一件事，而后者
			// 是必须付的成本：池子的拨号本来就带失败记忆（noteFailure → 判死 → 换一个）。
			// 先探测一遍等于把这段成本串行付两次，代价是"候选已知"到"有可用传输"之间
			// 白等两三秒 —— 而这段等待正好压在用户的第一个请求上。
			added := pool.AddNodes(extra)
			logger.Printf("entries: +%d from community (%s), pool now %d", added, src, pool.Len())
			if tr != nil {
				tr.FactInt("entries", pool.Len())
			}
		}()
	}

	// 4. 监听端口自动选择：先试 8080/1080，被占则顺延。用户通常不需要知道
	//    端口号 —— 系统代理自动指向选定值；--manual 下端口只用来打印。
	httpPort, err := pickPort(8080)
	if err != nil {
		fatal("pick http port: " + err.Error())
	}
	socksPort, err := pickPort(1080)
	if err != nil {
		fatal("pick socks port: " + err.Error())
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
		DialTimeout:    5 * time.Second,
		Trace:          tr,
	})
	if err := srv.Start(); err != nil {
		fatal("start proxy: " + err.Error())
	}

	// 5. 系统代理：只在接管时设置并拉起看门狗；--manual 只打印地址。
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

	logger.Printf("ready in %s. browse normally; Ctrl+C or close this window to stop (system proxy is restored).",
		time.Since(appStart).Round(time.Millisecond))
	tr.Mark("ready", fmt.Sprintf("after %s", time.Since(appStart).Round(time.Millisecond)))

	// 就绪之后才对服务端 DNS 那几个候选做一次探测。
	//
	// 它的产物不是"过滤候选"（这几个本来就要用），而是启动日志里那两行判据：ECH 当前
	// 是哪一态、以及 [probe] 的三段耗时。放在后台，用户不必等它 —— 而它也不再挡在
	// "隧道能不能建起来"前面（建隧道由下面的 Verify/Warm 直接做）。
	go func() {
		endProbe := tr.Begin("probe.total", 12*time.Second)
		pr := selector.Optimize(context.Background(), dnsNodes, host, cfg.InsecureEnabled())
		endProbe()
		tr.Mark("probe.done", fmt.Sprintf("%d/%d nodes", len(pr.Nodes), len(dnsNodes)))
		reportProbe(logger, tr, host, cfg.InsecureEnabled(), len(dnsNodes), pr)
	}()

	// 周期探活：每 10 分钟对池子头部 5 个节点做一次 TLS 握手（不带 WS 升级，
	// 升级会真的建 DO 烧额度）。运行期原本没有复探，节点死了要等真实流量撞上
	// 才知道 —— 空闲后的第一批请求就是那批撞墙的。探活是握手级的，探一次的
	// 成本远低于一次对冲拨号 + 重放；失败按既有账本记（5 次判死），不会冤死节点。
	// 探测权竞争由 RecheckTop 内部并发上限约束，不需要这里再限。
	probeStop := make(chan struct{})
	defer close(probeStop)
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-probeStop:
				return
			case <-t.C:
				if n := pool.RecheckTop(context.Background(), 5); n < 5 {
					logger.Printf("[probe] periodic recheck: %d/5 alive", n)
				}
			}
		}
	}()

	// 首次连通验证：一次真实的传输层建连（TLS+WS+auth），在后台跑，
	// 结果出来补一行日志 —— 部署是否健康，这一行就是最直接的回答。
	// *.workers.dev 在中国大陆被 SNI 阻断（实测同一 CF IP：workers.dev 的
	// ClientHello 被 RST，其他域名正常），这是"连不上"的头号原因，失败时
	// 直接把方向指给用户。
	go func() {
		// 先预热，再验证 —— 顺序是有代价的，不能反。
		//
		// 预热（TopUp）是"让请求能用"的那件事：它并行对冲拨号、建好的传输立刻挂进池子，
		// 用户请求马上就能用上。而 Verify 只是"这个部署健不健康"的提示行。
		// 原先是 Verify 在前，而它串行试最多 3 个节点、每个都可能耗满拨号超时
		// （坏窗口实测 6s 级）—— 于是预热被硬生生推迟十几秒，首个请求只能干等。
		// 现在改成：预热先跑，验证等它的结果（VerifyOnLive 不自己拨号）。
		pool.Warm()

		endVerify := tr.Begin("tunnel.verify", 10*time.Second)
		if node, err := pool.VerifyOnLive("www.google.com:443", 10*time.Second); err != nil {
			endVerify()
			tr.Mark("tunnel.failed", err.Error())
			logger.Printf("tunnel failed: %v", err)
			if strings.HasSuffix(host, ".workers.dev") {
				logger.Println("hint: *.workers.dev domains are SNI-blocked in mainland China (and ECH is not published for them).")
				logger.Println("      bind a custom domain to the Worker in the Cloudflare dashboard")
				logger.Println("      (Workers & Pages -> netmaster -> Settings -> Domains & Routes -> Add Custom Domain),")
				logger.Println("      then put that domain in config.json as \"server\". See docs/deployment.md.")
			}
		} else {
			endVerify()
			tr.Mark("tunnel.ok", "via "+node)
			logger.Printf("tunnel established via node %s", node)
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

// pickPort 从 preferred 开始顺延找第一个可用端口。
// 任何绑定失败都视为"这个端口用不了"继续顺延 —— Windows 上 EADDRINUSE 的
// errno 映射不可靠（errors.Is 匹配不到 WSAEADDRINUSE），而权限/防火墙之类的
// 失败换一个端口同样解决。全部失败时报最后一个真实原因。
// 先占住再立刻放开存在一个极小的竞态窗口（探测到 proxy.Start 之间被抢），
// 后果只是 Start 明确报端口占用，不会静默错绑。
func pickPort(preferred int) (int, error) {
	var lastErr error
	for i := 0; i < 100; i++ {
		p := preferred + i
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			ln.Close()
			return p, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("no free port from %d: %w", preferred, lastErr)
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
		fmt.Fprintln(os.Stderr, "watchdog can only be spawned by serve (missing NETMASTER_OWNER_PID)")
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
		fmt.Println("restore error:", err)
		os.Exit(1)
	}
	fmt.Println("system proxy restored")
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func usage() {
	fmt.Println("usage: netmaster <serve|nodes|restore> [flags]")
	fmt.Println("  serve   - run the local HTTP+SOCKS5 proxy and take over the system proxy")
	fmt.Println("  nodes   - keep fetching a URL through the proxy to watch whether the tunnel works")
	fmt.Println("  restore - restore the system proxy (after serve was killed uncleanly)")
	fmt.Print("\nConfig precedence: CLI flag > config.json > default. config.json mirrors the serve flags:\n" +
		"  { \"server\": \"<domain>\", \"password\": \"<password>\", \"manual\": false, \"rules\": \"\" }\n" +
		"Place it next to the binary or in %AppData%/netmaster/config.json.\n" +
		"Add -h to any subcommand for its own flags. Routing and exit logic: see docs/.\n")
}
