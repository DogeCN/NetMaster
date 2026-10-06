# 排障

服务端**没有任何 HTTP 诊断端点**——没有 `/health`、没有 `/stats`。浏览器打开 Worker 域名
只会得到 404 空 body，这是预期行为，不代表部署坏了。

判断部署是否健康的唯一直接信号是客户端 `ready in …` 之后补的这一行：

```
tunnel established via node 104.16.x.x
```

它是一次真实的 TLS + WebSocket + 首帧认证建流（目标 `www.google.com:443`，M0 实测后
从 `example.com:80` 改来——平台禁拨 80 端口、example.com 已迁 CF 网段，见
[m0-findings.md](m0-findings.md) E6/E7）。失败了会打 `tunnel failed: <原因>`。

## 连不上：先看这三件事

### 1. 口令不一致

`no password` / `tunnel failed` 里带 auth 字样时，先确认两端是同一个 PASSWORD：

```bash
npx wrangler secret list          # 看 Worker 上有没有 PASSWORD
printf '%s' '你的口令' | npx wrangler secret put PASSWORD
```

口令只在首帧里以 HMAC-SHA256 的形式出现，网络上没有明文——所以**服务端看不到你的口令，
也没法告诉你哪里错了**。认证失败的帧回 `0x01`，与格式错误、TS 超窗不可区分。

### 2. 系统时钟

首帧带 Unix 秒时间戳，服务端校验 **±300 秒**。客户端机器时钟偏了 5 分钟以上，所有连接都
会以 `0x01` 被拒——症状和"密码错了"一模一样。先对表。

### 3. 域名有没有绑上 / 是不是 workers.dev

Worker 只部署到 `workers.dev`，**custom domain 要你自己绑**。没绑域名时客户端的
`--server` 无解可解析，启动会直接报
`no entries: server domain unresolvable and community sources unreachable`。

绑了域名但填的是 `*.workers.dev`（或只按部署输出用了 workers.dev 域名）：**大陆网络下
SNI 阻断，客户端永远连不上**。实测（2026-10-03）：同一个 CF 边缘 IP，ClientHello 带
workers.dev 域名被 RST（`tls: EOF` / `connection was forcibly closed`），换其他域名
正常；workers.dev 的 HTTPS RR 不发布 ECH 配置，客户端的 ECH 兜底也启用不了。解法：
控制台给 Worker 加 Custom Domain（Workers & Pages → netmaster → Settings →
Domains & Routes → Add Custom Domain），把它填进配置的 `server`（首次启动交互输入，或 `--server`）。
客户端检测到 `*.workers.dev` 且验证失败时会打印这条提示。

## 看服务端日志

```bash
npx wrangler tail                  # 实时日志
npx wrangler tail --format json    # JSON 格式
```

日志默认静默。要打开：在 Cloudflare 控制台给 Worker 加变量 `DEBUG=1`（或
`wrangler.toml` 的 `[vars]` 里写 `DEBUG = "1"`），再 `wrangler deploy`。日志前缀：

```
[session] authenticated; stream 1 -> www.google.com:443
[session] direct exit failed (…); trying proxyip
[session] race slot 0 ProxyIP.HK.CMLiussss.net:443 failed: relay connect timeout (1502ms)
[session] stream 7 no first byte in 3000ms, forgetting route
[session] stream 7 connect failed: …; all proxyip exits failed
[router] flush dropped 3 rows: …
```

注意：`DEBUG` 是变量不是 Secret，改完要重新部署才生效。

中继测速**没有服务端日志**——它跑在 GitHub Actions 的 deploy run 里（"Probe relays"
步骤），不在 Worker 内。要看它的输出去 Actions 的 run 页面：逐条打印每个候选的成败与
原因（`ok … 5975ms` / `fail … (certificate has expired)`），末行是写进 KV 的排序结果。

## 症状对照

| 现象 | 含义 | 怎么办 |
|---|---|---|
| `no server: pass --server or set it in config / NETMASTER_SERVER` | 没给域名 | 交互输入一次即持久化，或加 `--server`；只想绕过用 `--local` |
| `no password: pass --password or set it in config / NETMASTER_PASSWORD` | 没给口令 | 同上 |
| `no entries: server domain unresolvable and community sources unreachable` | 域名解析不出来 **且** 三个社区源也全挂 | 先查域名绑定和本机 DNS |
| `entries: N (server DNS; community still pending)` | 只等到了服务端域名的 DNS 候选就先就绪了（这是常态，不是问题） | 社区源在后台并入，几秒后会补一行 `entries: +M from community` |
| `entries: +M from community (src), pool now K` | 社区优选已并入池子 | 正常；M=0 时说明社区源这次没给可用条目 |
| `entries: community gave nothing (src); staying on server DNS` | 社区源全挂，只剩域名 DNS 的候选 | 能连就不要紧；只有服务端域名也解析不出来才致命 |
| `tunnel failed: …` | 传输层建不起来 | 看上面"连不上"三件事 |
| `server rejected stream: target forbidden (0x02)` | 目标是私网 / CF 网段 / 端口 25 | 设计如此；私网应走客户端直连规则 |
| `server rejected stream: all exits failed (0x03)` | 直连失败且中继全挂 | 看服务端 tail 里的 race slot 失败原因 |
| `[route] <host> direct unusable (…) — switched to proxy and replayed` | 直连被判定不可用，已改走代理 | 正常自愈；该域名 30 分钟内不再试直连 |
| `[frag] <host> direct was blocked (…) — TLS fragmentation got through, staying direct` | 明文直连被拦，**分片直连穿过去了** | 最好的结果：留在直连，不多付一跳；该域名 6 小时内直连带分片起步 |
| `[frag] <host> plain direct was blocked — TLS fragmentation got through, staying direct` | 规则直连域名明文起步被拦，阶梯②补了枪分片、穿过去了 | 同上：留在直连并学会"这个域名要分片"（记 6 小时） |
| `[frag] <host> fragmented direct also blocked (…) — falling back to proxy` | 分片也穿不过去，已落代理 | 正常自愈；分片记忆同时清掉，下次直接走代理 |
| `[frag] <host> remembered fragmentation no longer works (…) — forgetting it` | 旧的分片记忆已失效 | 正常；不清它会让此后 6 小时每条连接白付约 400ms 再落代理 |
| `[frag] fragmented-direct disabled (--no-frag) …` | 启动时用了 `--no-frag` | 见下"站点对直连来源回 403" |
| `[route] <host> proxy tunnel dead (…) — switched exit and replayed` | 隧道建立但零字节即断，已换出口 | 正常自愈；Worker 侧也会把坏中继忘掉 |

## ECH：怎么知道现在是哪一态

ECH 用来隐藏 SNI。客户端在启动时把当前状态**明说**（`main.go` 的三态），这就是判据，不要
凭"文档说过它在隐"来假设：

| 日志 | 含义 | 怎么办 |
|---|---|---|
| `[ech] no ECH config published for this domain` | 域名的 HTTPS RR 里没有 `ech=`（zone 未开 Encrypted Client Hello） | 控制台 SSL/TLS → Edge Certificates 打开它 |
| `[ech] ECH handshake failed on every attempt — …VISIBLE SNI` | 有配置但握手没成 | 下看"回退链"，并跑 `echprobe` 复核 |
| `[ech] SNI hidden by ECH` | ECH 生效 | 不用管 |

2026-10-04 实测（`proxy.0xa.cc.cd`）是最后一态：`echprobe` 4/4 内层证书校验通过，同一边缘
IP 上明文 SNI 写本域 4/4 被 RST（换 `www.cloudflare.com` 同 IP 是 200）。

**握手级复核**（不只信启动日志）：

```bash
cd client && go run ./cmd/echprobe <域名>
# [1] 取 ECHConfig → [2a] 带校验的 ECH 握手 → [2b] 不校验 → [2c] 明文 TLS 对照
```

四步里 `[2a] OK` 才算"真的隐了"；`[2c]` 是判别"明文这条路通不通"的对照组。

## ECH 回退

兜底链（代码路径没变）：

1. 尝试 ECH（2 秒预算，走优选 IP；失败再试域名直连）；
2. 超预算或握手失败 → 退普通 TLS（明文 SNI），并触发 60 秒短路；
3. 60 秒后重新尝试 ECH——它随时可能恢复可用。

日志里看到 `tls: … (ech also failed — …)` 不必惊慌：那是两条路都失败时把 ECH 的原因一并
带出来，方便排查。只要连接本身成功了，说明至少普通 TLS 走通了。

个别站点既无 ECH 又被 IP + SNI 双拦，客户端无法本地绕过。

## 站点对直连来源回 403（如 arena.ai）

症状：某个站反复 403，而其他站正常；同一 URL 有时又通。这不是隧道问题——**是站点的 WAF
按来源 IP 拒绝**：客户端的"分片直连"阶梯会把 CF 承载的目标直接连出去（从你家宽带 IP），
站点回 403；而 403 是合法 HTTP 响应，阶梯的判据（首字节有没有回来）把它当成"直连成立"，
不回退到代理，还把 `fragDirect` 记 6 小时。

判据（两条命令就能定位）：

```bash
curl -sS -o /dev/null -w '%{http_code}
' https://<站点>/          # 本机直连：403？
curl -sS -x http://127.0.0.1:8080 -o /dev/null -w '%{http_code}
' https://<站点>/   # 经客户端
```

解法：

- **`netmaster serve --no-frag`**（或 config `"no-frag": true`）：关掉分片直连，这类站点
  直接走 Worker 中继；
- 或只针对该站：写一份规则文件（`DOMAIN,<站点>`）用 `--rules` 强制走代理。

## 节点全挂

客户端在节点池里随机起点轮询（`selector.Pool.Dial`），每个节点一条复用的 mux；节点 mux
失效时重拨一次；整池失败返回最后一个错误。

- 流被拒（`0x01`–`0x03`）是服务端裁决，换节点也是同一裁决，**不再轮询**——这是有意的，
  避免把一次失败放大成 N 次。
- 传输断开后自动重连：退避 1s → 2s → … → 32s（每次 ±20% 抖动）。断开期间新流进等待队列
  （上限 128 条、单条等 10 秒），重连后批量发起；超时或队列满则直接向调用方返回失败。
  不做会话恢复，在途流直接关闭，上层应用自行重试。
- 启动时的入口候选是 DNS 解析 + 社区源合并的结果，最多 64 个，再经延迟优选取最快 16 个
  进池。全挂退磁盘缓存，缓存也没有就只剩域名解析结果。
- 想观察实际选路效果：

```bash
netmaster nodes                                  # 持续发请求，看延迟/成功率
netmaster nodes --ipcheck https://api.ipify.org  # 统计观察到的出口 IP 分布
```

## 系统代理残留

`serve` 被强杀（任务管理器结束进程、断电）时来不及还原系统代理。三层保护：

1. **看门狗**：serve 成功接管系统代理后会派生一个脱离的 `netmaster watchdog` 子进程，它等
   owner 进程退出后自动还原（owner pid 走 `NETMASTER_OWNER_PID` 环境变量传递）；
2. **启动自愈**：下次启动先 `sysproxy.CleanupStale()`，日志会打
   `[sys] cleaned up stale system proxy from a previous unclean exit`；
3. **手动兜底**：

```bash
netmaster restore
```

Windows 上的实现细节：旧值存在 `%TEMP%/netmaster_sysproxy.json`，
`Enable()` 是"先落盘旧值、再逐项写注册表"。中途失败会回滚——不回滚的话注册表可能已被改了
一部分（比如 `ProxyEnable` 已置 1），系统代理会一直悬着指向本机端口。

**看门狗自己是可观测的**：它的每一步（监视哪个 pid、owner 何时退出、还原成功与否）都写在
`%AppData%/netmaster/watchdog`（256KiB 截断）。"自动还原没生效"先看这个文件——
看门狗没留下任何痕迹 = 它根本没被创建成功或随终端被连坐杀掉。

一个实测过的根因：从 Windows Terminal / VSCode 终端里启动 serve，终端把整棵进程树放进
Job Object（关窗即全杀），`DETACHED_PROCESS` 防不了 Job 连坐——看门狗和 serve 一起蒸发。
派生时已带 `CREATE_BREAKAWAY_FROM_JOB`（Job 不允许脱离则退回旧行为）；从这类终端跑长驻
serve，建议用 `start /b` 或计划任务把进程放进独立树。

三个平台各有实现，行为不同：

| 平台 | 实现 | 接管范围 | 状态文件 |
|---|---|---|---|
| Windows | 注册表 `Internet Settings` + WinINET 通知 | HTTP | `%TEMP%/netmaster_sysproxy.json` |
| macOS | `networksetup`（`-webproxy` / `-securewebproxy`） | HTTP/HTTPS，**不设 SOCKS** | `$TMPDIR/netmaster_sysproxy_darwin.json` |
| Linux | `gsettings`（`org.gnome.system.proxy.*`） | HTTP/HTTPS，**不设 SOCKS** | `$TMPDIR/netmaster_sysproxy_linux.json` |
| 其他（BSD 等） | 不支持 | — | 无（请用 `--manual`） |

差别要点：

- **macOS 只走 `networksetup`**，不碰 SystemConfiguration 私有 API（后者跨版本会碎）。服务名
  取 `networksetup -listallnetworkservices` 的全部条目；多网卡/有线环境若有异常，用
  `--manual` 自己填。
- **Linux 只走 `gsettings`**，覆盖 GNOME / Unity / Cinnamon 与 KDE Plasma 的 gsettings 后端。
  Xfce / MATE 等用别的 schema——最诚实的做法是明确告诉用户改用 `--manual`，而不是留一个
  半生效的设置。
- 两侧都不设 SOCKS：系统代理本身不转发 UDP，且多数应用的 SOCKS 支持要单独勾选，用户预期
  与实际差距大。
- 其他平台 `Enable()` 直接返回
  `sysproxy: system proxy takeover not supported on this platform (use --manual)`。

其他平台用 `--manual`，自己把 `http://127.0.0.1:<port>` 填进系统设置。

## 端口

默认先试 8080（HTTP）/ 1080（SOCKS5），被占则顺延（`pickPort` 最多试 100 个）。系统代理
自动指向选定值，所以你通常不需要知道端口号。`--manual` 下端口只用来打印：

```
manual mode. HTTP proxy: http://127.0.0.1:8080   SOCKS5: socks5://127.0.0.1:1080
```

## 开发期排障

```bash
# 服务端单测（真实退出码在最后一行）
cd server && node build.mjs && node test/crypto.mjs && node test/protocol.mjs && node test/integration.mjs

# 端到端：Go 客户端 ↔ Node devserver（同协议对端）
cd server && node test/devserver.mjs 0 devserver-password 0 &
cd client && go test ./internal/outbound -run TestProtoE2E -v

# devserver 单独调试：DBG=1 打印每帧
DBG=1 node test/devserver.mjs 0 devserver-password 0
```

`devserver.mjs` 的 CLI 是 `node test/devserver.mjs [port] [password] [denyLoopback]`，
第三参数传 `0` 放开回环目标（生产禁回环，测试要连本地 echo 服务器）。它启动后首行输出
`PORT=<实际端口>`。

## 相关

- 架构与出口选路：[architecture.md](architecture.md)
- 配额：[limitations.md](limitations.md)
- 部署运维：[operations.md](operations.md)
