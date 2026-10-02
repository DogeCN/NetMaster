# 发布操作单

打 tag 之后照着做。每一步都写了"怎么确认它成了"，没确认就不要进下一步。

版本口径：服务端（Worker）与客户端（二进制）同属一个 tag，`_worker.js` 与
`wrangler.toml` 随 Release 一起发；客户端没有自动更新，用户自行下载替换。

---

## 0. 打 tag 前的硬门

任何一条不满足，先别打 tag。

- [ ] **CI 全绿**：`.github/workflows/ci.yml`（构建 + 单测）与 `deploy.yml` 最近一次都是
      `success`。
- [ ] **验收真的跑过**：`acceptance.yml` 最近一次 `success`，且日志里
      `Live tunnel checks` 与 `CF-hosted target success rate` 是 **PASS 而不是 SKIP**。
      只看"绿色对勾"会被骗——工作流在没有 `NETMASTER_E2E_WORKER` 时会整段跳过 live 项，
      跳过的 run 也是绿的。
- [ ] **`server/wrangler.toml` 的 `crons` 是 `["0 * * * *"]`**（每小时）。验收期可能被临时
      改成 `*/10`，发版前必须改回来。
- [ ] **KV `id` 仍是占位符** `00000000000000000000000000000000`（32 个 0）。真实 id 由
      部署入口在部署时注入，入库文件里不该出现真值。
- [ ] **工作区干净**：`git status --short` 无输出；`main` 已包含 `v2` 的全部提交
      （`git rev-list --count main..v2` 为 0）。
- [ ] **没有残留的临时改动**：`git grep -nE "TODO|FIXME|临时|temporary" -- server/ client/`，
      逐条确认不是发布阻塞项。

## 1. 打 tag

```bash
git checkout main
git pull
git tag -a v0.2.0 -m "v0.2.0: protocol v2 (mux over WebSocket), Session/Router DO, ProxyIP exits"
git push origin v0.2.0
```

`release.yml` 由 tag 触发，自动：跑服务端测试 → 构建 `_worker.js` → 交叉编译五个平台的
客户端 → 按目标平台 `go vet` → 产物自检（体积过小直接失败）→ 跑客户端测试 → 创建 Release。

**确认**：

- [ ] Actions 里 `release` 工作流 `success`；
- [ ] Release 页面有 7 个资产：`_worker.js`、`wrangler.toml`、
      `netmaster-windows-amd64.exe`、`netmaster-linux-amd64`、`netmaster-linux-arm64`、
      `netmaster-darwin-amd64`、`netmaster-darwin-arm64`；
- [ ] Release notes 里的部署说明与实际一致（KV 占位符口径）。

## 2. 部署 Worker

三选一。共同的前置：一个 Cloudflare 账号；一个托管在 CF 的域名（客户端最终要连它）。

### 方式 A：GitHub Actions（推荐）

- [ ] 仓库 Secrets 已配 `PASSWORD`、`CLOUDFLARE_API_TOKEN`、`CLOUDFLARE_ACCOUNT_ID`；
- [ ] `deploy.yml` 跑过且 `success`（push 到 main 自动触发，或手动 `workflow_dispatch`）；
- [ ] 该工作流会自动解析/创建 KV namespace `netmaster` 并写入 `wrangler.toml` 的占位 id
      （只在 CI 的工作副本里改，不影响你的仓库）。

### 方式 B：本地一键脚本

```bash
export PASSWORD='<口令>'
scripts/deploy.sh --dry-run     # 先看它要做什么
scripts/deploy.sh
```

- [ ] 脚本打印的 KV id 是真实 id（不是 32 个 0）；
- [ ] 最后一步 `wrangler secret put PASSWORD` 成功。

### 方式 C：手动 wrangler

```bash
cd server
npm ci && node build.mjs
npx wrangler login
npx wrangler kv namespace create netmaster   # 输出的 id 填进 wrangler.toml
npx wrangler secret put PASSWORD
npx wrangler deploy
```

- [ ] `wrangler.toml` 里的 id 已换成真实值（否则 deploy 会以 id 非法失败）；
- [ ] 不需要 KV 时删掉整段 `[[kv_namespaces]]`：竞速退化为只用内置兜底列表，Cron 直接跳过。

### 绑定域名（三种方式都要做）

- [ ] Cloudflare 控制台 → Workers → 该 Worker → Settings → Domains & Routes → 绑定
      custom domain。这一步同时建 DNS 记录和路由，**是客户端能连上的全部前提**。

## 3. 部署后验证

- [ ] **协议层验收**：Actions → `e2e` → Run workflow，`worker` 填
      `<你的域名>`（或配 `NETMASTER_WORKER` secret），`mode=live`。看结论里
      5.x（CF 托管目标成功率）与 6.x（Router DO 复用）不是 SKIP。
- [ ] **客户端验收**：Actions → `acceptance` → Run workflow。三项必须全绿：
      `Protocol-level checks`、`Live tunnel checks (direct exit + mux)`、
      `CF-hosted target success rate`（PRD 要求 ≥99%）。
- [ ] **Cron 真的在写 KV**：`acceptance` 的 `KV health snapshot` 步骤里
      `proxyip:top` 与 `cron:lastRun` 都有值（`key not found` 说明 Cron 没跑成）。
- [ ] **客户端实机**：任选一台机器跑 `netmaster serve`，日志里出现
      `tunnel established via node <addr>`；浏览器能打开一个境外站点。
      这一行是"部署是否健康"最直接的回答——服务端没有任何 HTTP 端点。

## 4. 客户端分发

- [ ] 从 Release 下载对应平台的二进制；
- [ ] 写 `config.json`（放在可执行文件旁边，或 `%AppData%/netmaster/config.json`）：

```json
{ "server": "<你的域名>", "password": "<PASSWORD>" }
```

- [ ] 运行 `netmaster serve`（Windows 双击 exe 等价）。首次双击若无配置会生成模板；
- [ ] macOS 产物未签名，首次运行会被 Gatekeeper 拦住：
      `xattr -d com.apple.quarantine netmaster-darwin-arm64`；
- [ ] 退出时确认系统代理已还原（日志 `[sys] system proxy restored`）；被强杀时用
      `netmaster restore`。

## 5. 出问题怎么办

- **Worker 回滚**：Cloudflare 控制台 → Worker → Deployments → 选上一个版本 Rollback；
  或本地 `npx wrangler rollback`。回滚只影响服务端，客户端不需要动。
- **客户端回滚**：让用户重新下载上一个 tag 的二进制替换即可（无自动更新）。
- **系统代理残留**：`netmaster restore`；Windows 上旧值备份在
  `%TEMP%/netmaster_sysproxy.json`。
- **口令泄露**：`npx wrangler secret put PASSWORD` 换一个新口令，然后让所有客户端改
  `config.json`。两端不一致时客户端表现为连不上（服务端只会回 `0x01`，看不出原因）。

## 6. 发版后（可选清理）

- [ ] M0 探针退役：`gh workflow run m0-probe.yml --ref v2 -f teardown=true`，确认
      `netmaster-m0` worker 已删除；随后可删 `m0/` 与 `m0-probe.yml`（结论已固化在
      `docs/m0-findings.md`）。
- [ ] 确认 `acceptance.yml` 里的默认 worker 主机名与 KV namespace id 仍是本账号的
      （换账号部署的人需要改这两处）。
