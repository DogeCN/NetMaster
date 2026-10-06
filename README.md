# NetMaster

自建代理客户端 + Cloudflare Worker 服务端。服务端跑在 Workers **免费版**：一个
Worker、两个 Durable Object、一个 KV 命名空间。客户端是 Go 写的本地 HTTP + SOCKS5
代理，负责分流、入口候选与 ECH 出站。

KV 里那份中继健康排名由一个可选的 GitHub Actions 定时任务刷新（每小时左右）——
不开它也能用，见[架构](#架构)。

两端同属本仓库，协议也是自己的：WebSocket 之上的多路复用帧，连接级一次
HMAC 认证。没有用户标识符要生成、没有数据库 ID 要复制、没有订阅地址要粘贴，
也没有任何 HTTP 端点——口令两端各自本地派生成 16 字节签名，网络上不出现明文。

## 架构

```
浏览器 / 任意 TCP 应用
        │  SOCKS5 (127.0.0.1:1080)  ──  HTTP CONNECT (127.0.0.1:8080)
        ▼
  netmaster（本机 Go 客户端）
        │  分流：Clash 规则集 → geoip → 兜底；入口并发优选
        ├──────────────── 直连 ────────────────────────────▶ 目标站        2 跳
        │
        │  wss://  TLS(ECH，判据见限制一节) + WebSocket + 协议 v2 mux 帧
        ▼
  Cloudflare 边缘 Worker ──▶ Session DO（每连接一个，Hibernation）
                                  │  出口选路
                                  ├── 直连 connect() ───────────────────▶ 目标站   3 跳
                                  │
                                  └── ProxyIP 中继（按 SNI 转发）───────▶ 目标站   4 跳
                                        候选：会话缓存 → 顺序拨号(KV 里的测速排序)
```

跳数说明：直连是"客户端 → 目标"两跳；走服务端非 CF 托管目标是三跳；目标是
Cloudflare 承载的站点时，Workers 的 `connect()` 不能拨 CF 自己的 IP，只能借第三方
中继，四跳。**NAT64 出口已砍**——M0 实测 Workers `connect()` 不支持 IPv6 出站
（见 [docs/m0-findings.md](docs/m0-findings.md)）。

直连这一侧有一条**降级阶梯**：明文直连被拦（3 秒内零字节即断/静默丢弃）就改分片直连——
把 ClientHello 切成 8B 片、8ms 间隔，阻断设备靠重组读 SNI，重组窗口先到期它就只看到
碎片——再不行才落代理隧道。分片成功就**留在直连**：不多绕一跳，也不在服务端多烧一次
`connect()`。成败写进记忆（分片可行 6h / 该走代理 30min / TCP 失败 5min），日志打
`[frag]` / `[route]`，详见 [docs/routing.md](docs/routing.md)。

服务端出口侧：中继候选由**部署工作流测速排序**写进 KV（`proxyip:top`），运行期按序
**逐个**尝试（不再竞速），成功提前、失败置后，顺序变化写回 KV（每会话 3 次预算）。
详见 [docs/relay.md](docs/relay.md)。

## 快速开始

前提：一个 Cloudflare 账号、一个托管在 Cloudflare 的域名（客户端要连它）。全程只
涉及一个口令（PASSWORD）和一个域名，没有别的。

### 1. 部署服务端

**方式 A：fork 后走 CI（推荐）。**

1. fork 本仓库；
2. 在仓库 Settings → Secrets 里配三个 Secret：
   - `PASSWORD`——你定的口令，客户端要用同一个；
   - `CLOUDFLARE_API_TOKEN`——有 Workers 编辑权限的 API Token；
   - `CLOUDFLARE_ACCOUNT_ID`——账号 ID（控制台右侧可复制）；
3. push 到 `main`，`.github/workflows/deploy.yml` 自动完成：构建 → 解析/创建 KV
   namespace 并写进 `wrangler.toml` 的占位 id → `wrangler deploy`（DO migration 随
   部署自动应用）→ 把 `PASSWORD` 透传成 Worker Secret；
4. 在 Cloudflare 控制台给这个 Worker 绑定 custom domain。这一步同时建 DNS 记录和
   路由，是客户端能连上它的**全部前提**——Worker 只部署到 `workers.dev`，域名不归
   项目管。

   > **`*.workers.dev` 在中国大陆被 SNI 阻断，客户端直连必失败**（实测：同一 CF
   > 边缘 IP，ClientHello 带 workers.dev 域名即被 RST，换其他域名正常；且
   > workers.dev 不发布 ECH 配置，客户端的 ECH 兜底也无从启用）。绑定自己的
   > 域名是大陆用户的必做步骤，不是可选优化。症状对照：客户端日志
   > `tunnel failed: ... connection was forcibly closed / tls: EOF`。

**方式 B：本地一键脚本（推荐给不用 CI 的人）。**

```bash
cd <仓库根>
export PASSWORD='<你定的口令>'
scripts/deploy.sh              # 先加 --dry-run 可以只看它要执行什么
```

脚本做完整流程：`npm ci` → `node build.mjs` → 校验产物可加载 → 找/建 KV namespace
→ 生成临时配置 `wrangler.deploy.toml`（**不动入库的 `wrangler.toml`**）→
`wrangler deploy` → 透传 `PASSWORD`。它不会替你绑域名，最后一步仍要自己在控制台做。

**方式 C：手动 wrangler 部署。**

```bash
cd server
npm ci
node build.mjs                       # 生成 _worker.js（不要手改它，改 src/）
npx wrangler login
npx wrangler kv namespace create netmaster   # 把输出的 id 填进 wrangler.toml 的 [[kv_namespaces]]
npx wrangler secret put PASSWORD
npx wrangler deploy
```

注意：`wrangler.toml` 里的 KV `id` 是占位符（`0000…0000`），直接 `wrangler deploy`
会因 id 非法而失败——这是有意的，别拿占位符上线。deploy 工作流会按 title=netmaster
解析真 id 就地改写，本地手部部署参考 [docs/operations.md](docs/operations.md)。

### 2. 客户端

从 [Release](../../releases/latest) 下载对应平台的客户端
（`netmaster-windows-amd64.exe` / `netmaster-linux-amd64` / `netmaster-linux-arm64`），
或者自己构建：

```bash
cd client && go build -o netmaster.exe ./cmd/netmaster
```

不需要手写配置文件：**首次启动会交互式询问服务地址与口令**，写进
`%AppData%
etmaster
etmaster.json`（一份文件存所有持久化数据：配置、口令、
节点优选缓存）。也可以用 flag / 环境变量给：

```bash
./netmaster serve --server <你的域名> --password <PASSWORD>
./netmaster serve --local   # 纯绕过模式：直连/分片/ECH，不需要 Worker 与凭据
./netmaster --reset         # 清除全部持久化数据
```

Windows 双击 `netmaster-windows-amd64.exe` 与交互式首次启动等价；任何启动错误都会
等一次回车再关窗口，原因不会一闪而过。老版本放在程序目录的 `config.json` 会在首次
启动时自动迁移进 appdata 文件。

启动后监听端口自动选择（先试 8080 / 1080，被占则顺延），HTTP 入站监听
`127.0.0.1:8080`、SOCKS5 监听 `127.0.0.1:1080`。系统代理自动指向选定端口，退出时
自动还原；直接关闭窗口等于强杀进程，由看门狗子进程兜底还原。`ready in …` 日志之后
会补一行 `tunnel established via node <addr>`——**部署是否健康，这一行就是最直接的
回答**（服务端没有任何 HTTP 诊断端点）。

## 三个子命令

```
usage: netmaster <serve|nodes|restore|--reset> [flags]
  serve   - run the local HTTP+SOCKS5 proxy and take over the system proxy
            --local: bypass-only mode (direct/fragmented exits, no Worker)
  nodes   - keep fetching a URL through the proxy to watch whether the tunnel works
  restore - restore the system proxy (after serve was killed uncleanly)
  --reset - clear all persisted data (config, password, entry cache, lock)
```

| 子命令 | 作用 | 主要 flag |
|---|---|---|
| `serve` | 起本地代理、接管系统代理、退出还原 | `--server` `--password` `--manual` `--rules` `--no-ech` `--tunnels` `--frag-oob` `--no-frag` `--local` |
| `nodes` | 持续发请求，实时观察自适应选路 | `--server` `--password` `--target` `--ipcheck` |
| `restore` | 手动还原系统代理（serve 被强杀后用） | 无 |
| `--reset` | 清除全部持久化数据（配置、口令、优选缓存、单实例锁） | 无 |

`serve` 的 flag：`--server <domain>`、`--password <pw>`、`--manual`（不接管系统代理，
只打印监听地址）、`--rules <file>`（自定义分流规则文件）、`--no-ech`（关掉客户端→
Worker 这段链路的 SNI 隐藏）、`--tunnels <1-8>`（同时保持几条隧道）、`--frag-oob`
（分片首段改用 TCP 紧急数据发出，见下表 `frag-oob`）、`--no-frag`（关掉分片直连，
见下表 `no-frag`）、`--local`（纯绕过模式：直连/分片/ECH 出口，不建隧道、不需要凭据）。

`nodes` 的 flag：`--target <url>`（默认 `https://www.google.com/`）、
`--ipcheck <url>`（发请求到该 URL 并统计观察到的出口 IP 分布）。

配置优先级：**命令行 flag > 持久化文件 > 环境变量**。环境变量兜底用
`NETMASTER_SERVER` 与 `NETMASTER_PASSWORD`（后者为空时再退到 `PASSWORD`）。
加 `-h` 到任意子命令可看它自己的 flag。

## 配置说明

所有持久化数据在一个文件里：`%AppData%
etmaster
etmaster.json`（Linux/macOS 为
`os.UserConfigDir()/netmaster/netmaster.json`）。配置块的字段与 `serve` 的 flag
一一对应（名字去掉 `--`）：

```json
{
  "server": "<你的 Worker 域名>",
  "password": "<部署时设置的 PASSWORD>",
  "manual": false,
  "rules": "",
  "tunnels": 4
}
```

| 字段 | 类型 | 含义 |
|---|---|---|
| `server` | string | Worker 域名。写成 `https://nm.example.com/` 也会被剥掉 scheme 与路径 |
| `password` | string | 首帧 HMAC-SHA256 的密钥，不出网络 |
| `manual` | bool | true 时不接管系统代理，只打印监听地址 |
| `rules` | string | 自定义分流规则文件路径（Clash RULE-SET 格式，动作按文件名推断）；留空用内置规则集 |
| `tunnels` | int | 同时保持几条到边缘的隧道，1–8，省略时 4 |
| `no-ech` | bool | true 时关掉客户端→Worker 这段链路的 SNI 隐藏（默认开启）。仅在遇到偶发 bad handshake、需要对照实验时才需要改；本域明文 SNI 已被 RST（2026-10-04 实测），关掉很可能直接连不上 |
| `insecure` | bool | true 时不校验边缘证书（默认校验）。只给自建网关用自签证书的场景 |
| `frag-oob` | bool | true 时分片直连的**第一个分片**用 TCP 紧急数据（MSG_OOB）发出（默认关闭）。只对"普通分片也被拦"的站点有用：紧急指针会让阻断设备的重组失灵。代价：紧急字节是否进入对端字节流取决于 SO_OOBINLINE，个别接收方会拿到坏记录 —— 确认目标站点普通分片失效后再开 |
| `no-frag` | bool | true 时关掉分片直连：分流判代理的目标不再赌直连（直接走隧道），规则直连的目标被拦后直接改道。用于"站点 WAF 按来源 IP 拒绝直连"的场景（如 arena.ai：本机直连 403、经中继 200）——直连的成败判据只看传输层，看不见应用层 403，会误判成"直连成立" |

`tunnels` 为什么值得调：资源密集的页面（视频、图片流）一次会开几十条连接，多几条
隧道才不至于在服务端回收连接的瞬间整页超时（每条隧道一生约 30 次出站建连的预算）。
但每条常连隧道都按 Cloudflare DO 时长计费，免费版有每日上限。日常浏览留默认的 4
就行；如果你的账号额度吃紧、且主要做低频浏览，降到 2 能明显省额度。

文件里还有一块 `entry_cache`：节点优选的候选 IP 与时间，启动立刻进池不用等网络；
缓存缺失、超过 24 小时或条目太少时才在后台拉社区源并回写。老版本放在程序目录的
`config.json` 首次启动时自动迁移进这份文件，老文件留在原地不动。`netmaster --reset`
清除全部持久化数据。

## 限制与合规

- **WS 消息 ≈1:1 计入免费版每日 10 万请求**。M0 实测未观察到 20:1 折算，多路复用
  只省建连成本、不省消息量。协议层的对策：数据帧尽量满帧（单帧上限 64 KB）、
  心跳走 WebSocket 协议层 Ping（边缘自动应答，不计消息、不唤醒 DO）、没有逐帧 ACK。
- **Workers `connect()` 不支持 IPv6 出站**，NAT64 出口已从架构中移除；CF 承载目标的
  出口只剩 ProxyIP 中继一类。
- **公共中继的出口 IP 被 Cloudflare 系站点拉黑是常态**，部署测速 + 顺序记忆 + 会话亲和
  是自愈机制，不是根治。
- **ECH 生效，且对本域是必需项。** 2026-10-04 实测（本机、`proxy.0xa.cc.cd`，zone 的
  Encrypted Client Hello 一直开着）：DNS 的 HTTPS RR 发布 `ech=`；启动日志
  `[ech] SNI hidden by ECH`；仓库自带的 `go run ./cmd/echprobe <域名>` 连跑 4 轮全部
  内层证书校验通过（叶子证书 `*.proxy.0xa.cc.cd`）。对照组：同一边缘 IP 上明文 SNI 写这个
  域名 4/4 被 RST，而换 `www.cloudflare.com` 同一 IP 是 200 —— 明文那条路对本域是断的，
  所以"退化成可见 SNI"不再是"少一层保护"，而是连不上。
  判据只看启动日志三态（`no ECH config published` / `...VISIBLE SNI` / `SNI hidden by ECH`），
  不要假设它在隐。历史：2026-10-03 曾记录"完全不生效"（拿到外层 `cloudflare-ech.com`
  证书或直接 `server rejected ECH`），10-04 未能复现，按当时本机网络波动理解
  （未进一步验证）。
- geoip 的 CN 网段表只有 IPv4，纯 IPv6 站点一律按"非 CN"处理。
- 系统代理不转发 UDP：QUIC 不会被代理，建议在浏览器里禁用 QUIC
  （`chrome://flags/#enable-quic`），强制回落 TCP。

完整限额表与实测数据见 [docs/limitations.md](docs/limitations.md)。

> **合规提示**：在 Workers 上运行通用代理游走在 Cloudflare 服务条款边缘，账号可能
> 被封。请**个人使用、低流量**，用**小号部署**并做账号隔离、绑定自定义域名。本项目
> 仅供个人技术学习用途。

## 深入文档

| 文档 | 内容 |
|---|---|
| [docs/PRD.md](docs/PRD.md) | 产品需求文档（v1.1 冻结基线，文末附 v2 实施修订记录） |
| [docs/architecture.md](docs/architecture.md) | v2 架构、组件职责、协议帧格式、出口选路 |
| [docs/relay.md](docs/relay.md) | ProxyIP 中继：为什么需要、部署测速、顺序拨号、自建指引 |
| [docs/routing.md](docs/routing.md) | 客户端分流规则与优先级；直连降级阶梯（明文 → 分片 → 代理）与三层记忆 |
| [docs/limitations.md](docs/limitations.md) | 免费版限额表与实测数据 |
| [docs/operations.md](docs/operations.md) | 部署、运维、日志与 DEBUG |
| [docs/troubleshooting.md](docs/troubleshooting.md) | 排障：连不上、ECH 回退、节点全挂、系统代理残留 |
| [docs/m0-findings.md](docs/m0-findings.md) | M0 平台核验结论（1:1 计费、IPv6 出站等实测） |

## 开发

```bash
# 客户端
cd client && go build ./... && go vet ./... && go test ./...

# 服务端
cd server && npm ci && node build.mjs && npm test

# 单个服务端测试（真实存在的：crypto / integration / protocol / proxyip / order）
cd server && node --import ./test/shims/register.mjs test/order.mjs

# 端到端（Go 客户端 ↔ Node devserver，同协议对端）
cd server && node test/devserver.mjs 0 devserver-password 0 &
cd client && go test ./internal/outbound -run TestProtoE2E -v

# 端到端验收脚本（本地自检；给真实部署加 NETMASTER_E2E_WORKER / NETMASTER_E2E_PASSWORD 即跑全量）
# 经 socket.js 摸平台模块，要挂 Node shim —— 与 test-all.sh 同因
cd server && node --import ./test/shims/register.mjs test/e2e.mjs

# 分段耗时（启动/选路慢在哪；两侧都默认关闭，关闭时零开销）
NETMASTER_PROFILE=start.json netmaster serve   # 客户端：报告进 stdout，JSON 落文件
PROFILE=1 scripts/deploy.sh                    # 服务端：在临时配置上开 PROFILE=1，分段耗时写进 KV
node tools/profile.mjs compare a.json b.json   # 指纹不符 → exit 2，拒绝比"谁更快"
node tools/profile.mjs merge client.json server.json
```

采集器的打点、预算与 KV 键见 [docs/operations.md](docs/operations.md) 的"分段耗时采集"。

发版：打 `v*` tag 即构建全部 Release 产物（`.github/workflows/release.yml`）。
