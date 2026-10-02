# NetMaster

自建代理客户端 + Cloudflare Worker 服务端。服务端跑在 Workers **免费版**：一个
Worker、两个 Durable Object、一个 KV 命名空间、一个每小时一次的 Cron。客户端是
Go 写的本地 HTTP + SOCKS5 代理，负责分流、入口候选与 ECH 出站。

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
        │  wss://  TLS(ECH) + WebSocket + 协议 v2 mux 帧
        ▼
  Cloudflare 边缘 Worker ──▶ Session DO（每连接一个，Hibernation）
                                  │  出口选路
                                  ├── 直连 connect() ───────────────────▶ 目标站   3 跳
                                  │
                                  └── ProxyIP 中继（HTTP CONNECT）──────▶ 目标站   4 跳
                                        候选：会话缓存 → Router DO → 竞速(KV top4 + 内置兜底)
```

跳数说明：直连是"客户端 → 目标"两跳；走服务端非 CF 托管目标是三跳；目标是
Cloudflare 承载的站点时，Workers 的 `connect()` 不能拨 CF 自己的 IP，只能借第三方
中继，四跳。**NAT64 出口已砍**——M0 实测 Workers `connect()` 不支持 IPv6 出站
（见 [docs/m0-findings.md](docs/m0-findings.md)）。

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
会因 id 非法而失败——这是有意的，别拿占位符上线。无需 KV 时可以把
`[[kv_namespaces]]` 整段删掉：缺少 KV 绑定时竞速只用内置兜底列表、Cron 直接跳过，
功能不残。

### 2. 客户端

从 [Release](../../releases/latest) 下载对应平台的客户端
（`netmaster-windows-amd64.exe` / `netmaster-linux-amd64` / `netmaster-linux-arm64`），
或者自己构建：

```bash
cd client && go build -o netmaster.exe ./cmd/netmaster
```

写一份 `config.json` 放在可执行文件旁边（或 `%AppData%/netmaster/config.json`）：

```json
{ "server": "<你的域名>", "password": "<PASSWORD>" }
```

然后：

```bash
./netmaster serve          # Windows 双击 netmaster-windows-amd64.exe 等价
```

双击启动的细节：首次双击若没有任何 config.json，会在 exe 旁边生成一份模板，填好
两个值再点一次即可；任何启动错误都会等一次回车再关窗口，原因不会一闪而过。

启动后监听端口自动选择（先试 8080 / 1080，被占则顺延），HTTP 入站监听
`127.0.0.1:8080`、SOCKS5 监听 `127.0.0.1:1080`。系统代理自动指向选定端口，退出时
自动还原；直接关闭窗口等于强杀进程，由看门狗子进程兜底还原。`ready in …` 日志之后
会补一行 `tunnel established via node <addr>`——**部署是否健康，这一行就是最直接的
回答**（服务端没有任何 HTTP 诊断端点）。

## 三个子命令

```
usage: netmaster <serve|nodes|restore> [flags]
  serve   - run the local HTTP+SOCKS5 proxy and take over the system proxy
  nodes   - keep firing requests and watch adaptive exit selection live
  restore - restore the system proxy (after serve was killed uncleanly)
```

| 子命令 | 作用 | 主要 flag |
|---|---|---|
| `serve` | 起本地代理、接管系统代理、退出还原 | `--server` `--password` `--manual` `--rules` |
| `nodes` | 持续发请求，实时观察自适应选路 | `--server` `--password` `--target` `--ipcheck` |
| `restore` | 手动还原系统代理（serve 被强杀后用） | 无 |

`serve` 的 flag：`--server <domain>`、`--password <pw>`、`--manual`（不接管系统代理，
只打印监听地址）、`--rules <file>`（自定义分流规则文件）。

`nodes` 的 flag：`--target <url>`（默认 `https://www.google.com/`）、
`--ipcheck <url>`（发请求到该 URL 并统计观察到的出口 IP 分布）。

配置优先级：**命令行 flag > config.json > 环境变量**。环境变量兜底用
`NETMASTER_SERVER` 与 `NETMASTER_PASSWORD`（后者为空时再退到 `PASSWORD`）。
加 `-h` 到任意子命令可看它自己的 flag。

## 配置说明

`config.json` 的字段与 `serve` 的 flag 一一对应（名字去掉 `--`）：

```json
{
  "server": "<你的 Worker 域名>",
  "password": "<部署时设置的 PASSWORD>",
  "manual": false,
  "rules": ""
}
```

| 字段 | 类型 | 含义 |
|---|---|---|
| `server` | string | Worker 域名。写成 `https://nm.example.com/` 也会被剥掉 scheme 与路径 |
| `password` | string | 首帧 HMAC-SHA256 的密钥，不出网络 |
| `manual` | bool | true 时不接管系统代理，只打印监听地址 |
| `rules` | string | 自定义分流规则文件路径（Clash RULE-SET 格式，动作按文件名推断）；留空用内置规则集 |
| `latencyToleranceMs` | int | 出口优选容差（当前版本仅供配置解析，出口选择逻辑尚未消费） |

查找顺序：`./config.json`，然后 `%AppData%/netmaster/config.json`（Linux/macOS 为
`os.UserConfigDir()`）。两个位置都没有不是错误——全部配置也可以由 flag 给出。文件
存在但 JSON 损坏是错误：静默忽略一份读不出来的配置，会让人以为它生效了。

## 限制与合规

- **WS 消息 ≈1:1 计入免费版每日 10 万请求**。M0 实测未观察到 20:1 折算，多路复用
  只省建连成本、不省消息量。协议层的对策：数据帧尽量满帧（单帧上限 64 KB）、
  心跳走 WebSocket 协议层 Ping（边缘自动应答，不计消息、不唤醒 DO）、没有逐帧 ACK。
- **Workers `connect()` 不支持 IPv6 出站**，NAT64 出口已从架构中移除；CF 承载目标的
  出口只剩 ProxyIP 中继一类。
- **公共中继的出口 IP 被 Cloudflare 系站点拉黑是常态**，动态列表 + 竞速 + 亲和记忆
  是自愈机制，不是根治。
- ECH 在部分网络下间歇失败（服务端返回 outer 名证书），客户端有 2s 尝试预算与
  60s 短路兜底。
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
| [docs/relay.md](docs/relay.md) | ProxyIP 中继：为什么需要、候选来源、竞速、自建指引 |
| [docs/routing.md](docs/routing.md) | 客户端分流规则与优先级 |
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

# 单个服务端测试（真实存在的：cron / crypto / integration / protocol / proxyip / race / router）
cd server && node test/router.mjs

# 端到端（Go 客户端 ↔ Node devserver，同协议对端）
cd server && node test/devserver.mjs 0 devserver-password 0 &
cd client && go test ./internal/outbound -run TestProtoE2E -v

# 端到端验收脚本（本地自检；给真实部署加 NETMASTER_ENDPOINT / NETMASTER_PASSWORD 即跑全量）
cd server && node test/e2e.mjs
```

注意：`scripts/test-all.sh` 的测试清单仍指向已不存在的 `control` / `relay`
（`.github/workflows/ci.yml` 已更新为 v2 列表），本地全量脚本跑不通——见
[docs/operations.md](docs/operations.md)。

发版：打 `v*` tag 即构建全部 Release 产物（`.github/workflows/release.yml`）。
