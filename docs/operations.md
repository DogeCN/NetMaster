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
`wrangler deploy` 会因 id 非法而失败——**这是有意的：别拿占位符上线**。不需要 KV 时把
`[[kv_namespaces]]` 整段删掉即可：缺少 KV 绑定时竞速只用内置兜底列表，功能不残。

`keep_vars = false` 是显式写出的：部署时删除 Worker 上已存在、但本文件里没有的变量。一份
不描述现实的配置文件比没有配置文件更糟。

## 部署产物

`_worker.js` 由 `server/build.mjs` 把 `src/` 下 9 个模块拼接成一个文件
（crypto → protocol → exits → proxyip → race → router → session → index）。
它是**生成物**，改 `src/` 之后重新 `node build.mjs`。

`build.mjs` 同时是 CI 的语法门：`src/` 里任何一个走错字符都会在这里失败，而不是在生产里。

## 环境变量

| 变量 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `PASSWORD` | Secret | — | 首帧 HMAC 密钥。必需 |
| `DEBUG` | var | 空 | `'1'` 打开 `[session]` / `[router]` 日志，配合 `wrangler tail` |
| `RACE_SLOTS` | var | 6 | 竞速槽位数 |
| `RACE_SLOT_TIMEOUT_MS` | var | 1500 | 单槽超时 |
| `RACE_GLOBAL_TIMEOUT_MS` | var | 3000 | 竞速全局超时 |
| `RACE_STAGGER_MS` | var | 120 | 槽位交错启动间隔 |
| `RACE_KV_TOP` | var | 4 | 从 KV 取前 N 个中继 |

竞速参数非法或 ≤0 一律回落到默认（`posEnv`）：写错一个字符不该变成 0ms。

`DEBUG` 是变量不是 Secret——改完要重新部署。

## 存储与迁移

| 绑定 | 类型 | migration tag | 说明 |
|---|---|---|---|
| `SESSION` | Durable Object（内存） | `v1` | 每条 WS 一个实例，不落盘 |
| `ROUTER` | Durable Object（SQLite） | `v2` | 全局单实例，存目标 → 出口映射 |
| `KV` | KV namespace | — | 订阅更新任务写 `proxyip:top`（见下节） |

Migration 随 `wrangler deploy` 自动应用，不需要额外步骤。

Router DO 的写入是"先返回、后落盘"：攒 5 秒或 50 条 flush 一次（DO Alarm 驱动），队列空了
就撤掉待定 Alarm，避免空转唤醒消耗行写配额。flush 失败重试 1 次，仍失败就丢弃该批——缓存
可以从竞速重建，不值得为它反复写。**进程驱逐时未 flush 的映射接受丢失。**

`forget` 不排队，立即生效：映射被证伪时必须马上删，不能跟着队列一起等 5 秒，否则下一条流
会踩同一脚。

## 中继池刷新（GitHub Actions 定时任务，可选）

`proxyip:top` 是竞速用的"当前最健康的 4 个中继"。它由
`.github/workflows/refresh-relays.yml` 刷新：**每小时左右**跑一次
`server/tools/refresh-relays.mjs`，拉社区源 + 内置兜底 → 并发探测（CONNECT
握手，3s 超时）→ 按成功率/延迟排序 → 取前 4 写进 KV，last-good-wins（本轮全败
就不写，保留上一轮）。

### 默认不开，也不影响使用

这一点值得说清：

- **不启用这个定时任务，代理照样能用。** KV 为空时竞速只用内置兜底列表
  （`proxyip.js` 的 9 条 CMLiussss），功能不残，只是候选质量差一些、命中率
  低一些。
- **启用之后**，KV 里有新鲜的健康排名，竞速把 KV top4 排在候选最前，
  CF 承载目标的出口质量明显更好。
- **GitHub 的 `schedule` 是"每小时左右"而不是准点**（官方说明有几分钟级延迟，
  高峰期可能更久）。看到 KV 时间戳比整点晚几分钟是**正常的**，不是故障。
  另外仓库连续 60 天无活动时 GH 会自动停用定时任务——长期不用的 fork 别指望它。

启用：给仓库配两个 Secret（`CLOUDFLARE_API_TOKEN` 需 **KV 写**权限、
`CLOUDFLARE_ACCOUNT_ID`），然后手动 `workflow_dispatch` 跑一次看输出；
建议先勾 `dry_run=true` 确认它会写什么。

### 用到的 KV 键

| 键 | 内容 |
|---|---|
| `proxyip:top` | 前 4 名中继（竞速读它） |

只有这一个键，没有游标、没有 pending：探测是在 runner 上一次性跑完的，不存在
"分批续跑"的中间状态。

### 排障

run 页面就是唯一的排障现场，日志里逐条打印每个候选的成败与原因：

```
source https://ipdb.api.030101.xyz/?type=bestproxy: 10 entries
source builtin:cmliussss: 9 entries
probing 19 relays (concurrency 8, timeout 3000ms, target cp.cloudflare.com:80)
probes: 3/19 usable
  ok   ProxyIP.HK.CMLiussss.net:443 182ms
  fail 8.218.70.238:443 46ms (CONNECT status 405)
last-good-wins: no usable relay this round (19 probed) — keeping the previous proxyip:top
```

**绿灯不等于池子被刷新过**：本轮全败时也退出 0（定时任务红了会让人习惯性忽略），
所以要认 `last-good-wins: …` 这一行，而不是只看 run 是不是绿的。

本地干跑（真拉源 + 真探测，不写 KV）：

```bash
cd server && DRY_RUN=1 node tools/refresh-relays.mjs
```

## 构建与测试

```bash
scripts/test-all.sh                                  # 全量（服务端 + 客户端）

cd server && npm ci && node build.mjs                # 服务端构建
node test/crypto.mjs   # 鉴权与 TS 窗口
node test/protocol.mjs # 帧编解码
node test/integration.mjs
node test/proxyip.mjs  # 中继 CONNECT、兜底列表、健康记忆
node test/race.mjs     # 竞速与候选组装
node test/router.mjs   # Router DO 队列与 flush
node test/refresh-relays.mjs  # 中继池刷新脚本（替代 Worker Cron）

cd client && go vet ./... && go test ./...           # 客户端

# 端到端（Go 客户端 ↔ Node devserver）
cd server && node test/devserver.mjs 0 devserver-password 0 &
cd client && go test ./internal/outbound -run TestProtoE2E -v
```

注意：`scripts/test-all.sh` 里 crypto/protocol 是纯逻辑直跑，其余套件
（integration / proxyip / race / router / refresh-relays）经 `socket.js` 摸平台模块，
脚本会挂 `NODE_OPTIONS="--import ./test/shims/register.mjs"`——单跑这些测试时同样要挂，
否则 `ERR_UNSUPPORTED_ESM_URL_SCHEME`。

`scripts/test-all.sh` 与各 workflow 都用 `set -o pipefail`——node 的报错走 stderr，
`tail` 会吞掉退出码，不加这个测试挂了 CI 也是绿的。

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
[session] race slot 0 ProxyIP.HK.CMLiussss.net:443 failed: …
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
