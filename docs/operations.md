# 运维

## 部署要配什么

**三个 GitHub Secret，一次 push，然后自己绑域名。** 没有别的。

| Secret | 用途 |
|---|---|
| `CLOUDFLARE_API_TOKEN` | CI 调 Cloudflare API（需 Workers Scripts 编辑权限） |
| `CLOUDFLARE_ACCOUNT_ID` | 账号 ID |
| `PASSWORD` | 客户端连接口令，CI 透传成 Worker Secret |

```bash
gh secret set PASSWORD
gh secret set CLOUDFLARE_API_TOKEN
gh secret set CLOUDFLARE_ACCOUNT_ID
```

push 到 `main` 后 `.github/workflows/deploy.yml` 自动跑：checks（构建 + 单测）→ 构建 bundle
→ 通过 CF REST API 解析/创建同名 KV namespace 并把真实 id 写进 `wrangler.toml` 的占位符
→ `wrangler deploy`（DO migration 随部署自动应用）→ 把 `PASSWORD` 透传成 Worker Secret。

也可以 `workflow_dispatch` 手动触发。

**没有部署后验证**：CI 的网络环境和用户差别很大（runner 在美国，用户在大陆），runner 能通
不代表用户能通，反过来也一样。部署是否健康由"客户端能不能连上"直接回答——Worker 没有任何
HTTP 端点来回答这个问题。

### 域名绑定不归项目管

Worker 只部署到 `workers.dev`。Custom domain 是用户在自己的 zone 上绑的（控制台或
`wrangler` 均可），这一步同时建 DNS 记录和路由，是客户端能连上它的**全部前提**。项目不
持有域名，也就没有"改一行 routes"这类配置。

### 未设 PASSWORD 会怎样

部署仍会成功，但 Worker 会拒绝一切连接（首帧 HMAC 对不上）。CI 会打一条 warning。补上：

```bash
printf '%s' '你的口令' | npx wrangler secret put PASSWORD
```

注意 `printf` 而不是 `echo`：`echo` 会追加一个换行符，那个换行会变成口令的一部分。

## 本地部署

**推荐：用一键脚本。** 它会自己建/复用 KV namespace、生成临时配置（不动入库的
`wrangler.toml`）、部署并透传 `PASSWORD`：

```bash
export PASSWORD='<你的口令>'
scripts/deploy.sh                 # 或先 scripts/deploy.sh --dry-run 看它要做什么
```

脚本用 `wrangler deploy -c wrangler.deploy.toml`，那份临时配置是 `wrangler.toml` 的副本加
真实 namespace id，已在 `.gitignore` 里——**为什么不直接改写 `wrangler.toml`**：它是入库
文件，就地改会污染工作区（`git status` 多一个改动、下次 `pull` 可能冲突）。CI 里可以就地
`sed`，因为 runner 是一次性的。

手动部署：

```bash
cd server
npm ci
node build.mjs                              # 生成 _worker.js（改 src/，不要手改产物）
npx wrangler login
npx wrangler kv namespace create netmaster  # 输出的 id 填进 wrangler.toml 的 [[kv_namespaces]]
npx wrangler secret put PASSWORD
npx wrangler deploy
```

`wrangler.toml` 里的 KV `id` 是占位符 `00000000000000000000000000000000`，直接
`wrangler deploy` 会因 id 非法而失败——**这是有意的：别拿占位符上线**（deploy 工作流会
按 title=netmaster 解析真 id 就地改写）。

`keep_vars = false` 是显式写出的：部署时删除 Worker 上已存在、但本文件里没有的变量。一份
不描述现实的配置文件比没有配置文件更糟。

## 部署产物

`_worker.js` 由 `server/build.mjs` 把 `src/` 下 10 个模块拼接成一个文件
（`crypto / protocol / exits / socket / proxyip / order / profile / session / index`）。
它是**生成物**，改 `src/` 之后重新 `node build.mjs`。

模块顺序**从 import 图 DFS 推导**，不是手写的数组：顺序写死过一次，代价是"新增的
`src/*.js` 忘了登记就被静默丢掉，而 CI 的 `node --check` 只查语法、照样全绿"——本轮的
`profile.js` 就正站在这个雷上（`session.js` import 了它、build 也"成功"，而
`makeProfiler` 一次都没被调用）。现在缺失模块与别名 import 一律硬失败。

`build.mjs` 同时是 CI 的语法门：`src/` 里任何一个走错字符都会在这里失败，而不是在生产里。

## 环境变量

| 变量 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `PASSWORD` | Secret | — | 首帧 HMAC 密钥。必需 |
| `DEBUG` | var | 空 | `'1'` 打开 `[session]` 日志，配合 `wrangler tail` |
| `ORDER_SLOTS` | var | 6 | 顺序拨号的候选上限 |
| `ORDER_SLOT_TIMEOUT_MS` | var | 5000 | 单条候选尝试上限 |
| `PROFILE` | var | 关 | `'1'` 时会话结束把**分段耗时**写进 KV（见下"分段耗时采集"）。花 KV 写配额，每会话最多 3 次 |

顺序参数非法或 ≤0 一律回落到默认（`posEnv`）：写错一个字符不该变成 0ms。

`DEBUG` 与 `PROFILE` 都是变量不是 Secret——改完要重新部署。`scripts/deploy.sh` 支持
`PROFILE=1` 前缀，把它加在**部署时生成的临时配置**上；不要在控制台手改：`keep_vars = false`
会在下次 `wrangler deploy` 时把控制台里有、文件里没有的变量悄悄删掉，于是"部署时明明好的"
下次就没了。

## 存储与迁移

| 绑定 | 类型 | migration tag | 说明 |
|---|---|---|---|
| `SESSION` | Durable Object（内存） | `v1` | 每条 WS 一个实例，不落盘 |
| `KV` | KV namespace | — | `proxyip:top`（中继候选，见下节）+ `profile:*`（度量） |

Migration 随 `wrangler deploy` 自动应用，不需要额外步骤。

迁移历史只能**追加**、不能改写——这是 2026-10-06 用三次红 CI 换来的教训：Router DO 移除时
先删配置、再删迁移、最后补回，分别撞上 10074（SessionDO already exists，配置缺 v2 触发全量
重放）与 10064（class is depended on by existing Durable Objects，线上有存量实例）。正确姿势
是保留 v2（创建）、追加 v3（`deleted_classes = ["RouterDO"]`）。

## 中继测速（部署时）

`proxyip:top` 是顺序拨号用的"已测速排序的中继候选"。它**只在部署时写入**：
`deploy.yml` 的 "Probe relays and write ranked order to KV" 步骤，候选硬编在工作流的
`RELAYS` 环境变量（9 个 CMLiussss 域名型），由 `server/tools/probe-relays.mjs` 逐条做
`PROBE_TIMES=2` 次真实 TLS 握手（证书校验通过才算可用），可用优先、延迟升序，
取前 `RELAY_TOP_N=6` 条写进 KV。

曾经有过一个每小时刷新的定时任务（refresh-relays.yml），2026-10-06 连 Worker Cron 一起
停用：KV 写配额被打爆过一次，而部署是低频、可控、天然带"部署即验证"语义的写入时机。
运行期仍会继续修正顺序——Session 的 `persistRelayOrder` 在顺序变化时写回 KV
（每会话 3 次预算），这是唯一的运行期写入。

### 用到的 KV 键

| 键 | 内容 | 谁写 |
|---|---|---|
| `proxyip:top` | 有序中继候选（JSON 数组） | 部署工作流 + 运行期重排（有预算） |
| `profile:*` | 分段耗时度量 | 会话结束/按条数触发（每会话 3 次预算） |

### 排障

排障现场是 Actions 的 deploy run 日志，"Probe relays" 步骤逐条打印每个候选的成败与原因：

```
probed 9 relays (times=2, target=www.cloudflare.com:443):
  ok   ProxyIP.Oracle.CMLiussss.net:443 5975ms
  fail ProxyIP.Aliyun.CMLiussss.net:443 5984ms (certificate has expired)
  fail ProxyIP.HK.CMLiussss.net:443 6000ms (timeout)
proxyip:top = [{"host":"ProxyIP.Oracle.CMLiussss.net",...}]
KV proxyip:top written (3 entries)
```

本地干跑（真探测，不写 KV；KV_ID 缺省时自动跳过写入环节前先校验）：

```bash
cd server && DRY_RUN=1 RELAYS="ProxyIP.KR.CMLiussss.net ..." node tools/probe-relays.mjs
```

## 构建与测试

```bash
scripts/test-all.sh                                  # 全量（服务端 + 客户端）

cd server && npm ci && node build.mjs                # 服务端构建
node test/crypto.mjs   # 鉴权与 TS 窗口
node test/protocol.mjs # 帧编解码
node test/integration.mjs
node test/proxyip.mjs  # 中继 CONNECT 与解析
node test/order.mjs    # 顺序出口：成功提前/失败置后/写回预算

cd client && go vet ./... && go test ./...           # 客户端

# 端到端（Go 客户端 ↔ Node devserver）
cd server && node test/devserver.mjs 0 devserver-password 0 &
cd client && go test ./internal/outbound -run TestProtoE2E -v
```

注意：`scripts/test-all.sh` 里 crypto/protocol 是纯逻辑直跑，其余套件
（integration / proxyip / order）经 `socket.js` 摸平台模块，
脚本会挂 `NODE_OPTIONS="--import ./test/shims/register.mjs"`——单跑这些测试时同样要挂，
否则 `ERR_UNSUPPORTED_ESM_URL_SCHEME`。

`scripts/test-all.sh` 与各 workflow 都用 `set -o pipefail`——node 的报错走 stderr，
`tail` 会吞掉退出码，不加这个测试挂了 CI 也是绿的。

## 分段耗时采集（profile）

"这次改动到底快在哪、慢在哪"要有**可比较的证据**。两侧各一套采集器，都默认关闭，
关闭时零开销（调用点是 nil 接收者上的空操作，热路径上连一个 `if` 都不留）。

**客户端**：`NETMASTER_PROFILE=1`（或 `=stdout`）把报告打到 stdout；
`NETMASTER_PROFILE=<路径>` 额外把 JSON 落到该文件。报告直接走 stdout 而不是 logger
——logger 每行带时间戳，而报告自己就有偏移列。

```bash
NETMASTER_PROFILE=start.json netmaster serve
```

打点覆盖启动的每一段，**超出自己预算的段在报告里标 `!`**（真正贵的那一段往往不是你以为的
那一段，所以预算是"这段超过它就不对"而不是"参考值"）：`entries`（入口候选拉取，带来源与
条数）、`rules.load`（预算 500ms）、`probe.total`（12s）、`tunnel.verify`（10s）、
`ready`（从进程启动算起）、`ech`。请求路径上还有 `connect.<rung>`、`replay.*`、`frag.*`。

**服务端**：`PROFILE=1` 时会话结束把分段耗时写进 KV，键 `profile:<target>:<minute>`、
TTL 1 小时，**每会话最多 3 次写，且预算全会话共享**（按目标各算的话一次首屏就能写几十次
KV，而"客户端疯狂开页面"恰恰是最不该烧配额的场景）。写 KV 而不打日志的原因见
[m0-findings](m0-findings.md) E9：DO 内的 console 在 `wrangler tail` 上**完全不可见**。

**指纹**：facts 覆盖节点池身份、`tunnels`、`ech`、`insecure`、`rules-file`、frag 参数
（`Chunk/Delay/MaxSpan`）与 build。分片参数一改、节点池一换，两次运行就不可比。

```bash
node tools/profile.mjs compare a.json b.json           # 两次客户端运行；不可比 → exit 2
node tools/profile.mjs merge    client.json server.json  # 双端合并成一条时间线
node tools/profile.mjs fingerprint doc.json
```

工具的第一职责不是画图而是**防止"比错了"**：指纹不同就逐项点名差异、拒绝给出"谁更快"
（exit 2）。本项目吃过一次亏——入口池改动前用了 `nodes[:64]` 随机截断，两次跑的节点池根本
不是一回事，"7.2s → 5.1s"作废。服务端记录还带出口阶梯分布
（`0=直连 1=会话缓存 2=顺序中继`）：期望走 0 却总落在 2，说明"直连被判死"的判断偏保守。

两端 span 单位不同：客户端是 Go `time.Duration`（**纳秒**整数），服务端是 `ms`。工具会折算，
手工对齐时别忘了——不折算的话客户端每个阶段都显示成 0。

## 发版

打 `v*` tag 即触发 `.github/workflows/release.yml`：构建 `_worker.js` 与 `wrangler.toml`，
交叉编译五个平台的客户端（windows-amd64 / linux-amd64 / linux-arm64 / darwin-amd64 /
darwin-arm64），按目标平台逐个 `go vet`、跑两端测试、做产物自检（体积异常小直接失败），
创建 Release。标签重打时旧 Release 会被删除重建，保证 `vX.Y.Z` 的产物永远来自该标签当前
指向的提交。

```bash
git tag v0.2.0 && git push --tags
```

macOS 产物未签名：首次运行会被 Gatekeeper 拦住，右键 → 打开，或
`xattr -d com.apple.quarantine <文件名>`。

## 日志口径

服务端（DEBUG=1，`wrangler tail`）：

```
[session] authenticated; stream 1 -> www.google.com:443
[session] direct exit failed (…); trying proxyip
[session] router relay X failed: …
[session] relay ProxyIP.HK.CMLiussss.net:443 failed: …
[session] stream 7 no first byte in 3000ms, forgetting route
[router] flush dropped 3 rows: …
```

客户端（stdout，`log.Ltime` 时间戳）：

```
config: D:\...\config.json
entries: 51 (community: net)
[probe] 51 entries -> 16 nodes in 1.4s
rules: 8421 entries (fetched, skipped 312 lines)
[geoip] CN ranges ready: 4312 (cache)
[proxy] HTTP  proxy on 127.0.0.1:8080
[proxy] SOCKS5 proxy on 127.0.0.1:1080
[sys] system proxy ON -> http://127.0.0.1:8080 (auto-restored on exit)
[sys] watchdog started (pid 12345) to auto-restore on crash
ready in 1.2s. browse normally; Ctrl+C or close this window to stop (system proxy is restored).
tunnel established via node 104.16.x.x
```

客户端运行时输出一律英文。

## M0 遗留物

已全部清理完毕（2026-10-03）：探针 worker `netmaster-m0` 已删除，`m0/` 目录与
`m0-probe.yml` / `live-debug.yml` / `cron-debug.yml` 已从仓库移除，临时 SSH key 已撤销。
M0 结论固化在 [m0-findings.md](m0-findings.md)。
