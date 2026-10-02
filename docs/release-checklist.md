# 发布操作单

从"代码就绪"到"Release 页面上线"的完整顺序。每一步都写了**怎么确认它成了**——没确认就
不要进下一步。

版本口径：服务端（Worker）与客户端（二进制）同属一个 tag；`_worker.js` 与 `wrangler.toml`
随 Release 一起发。客户端没有自动更新，用户自行下载替换。

顺序一句话：**合并 → 部署（自动）→ 验证 → 打 tag → 再验证 → 清理**。
先部署再打 tag 是刻意的：Release 页面上的 `_worker.js` 必须对应一个已经在跑的版本。

---

## 0. 打 tag 前的硬门

任何一条不满足，先别打 tag。

- [ ] **工作区干净**：`git status --short` 无输出。未提交的改动会让人以为发布的是 A，
      实际合并的是 B。
- [ ] **合并已完成**：`main` 已包含 `v2` 的全部提交
      （`git rev-list --count main..v2` 为 0）。
- [ ] **CI 全绿**：`ci.yml`（构建 + 单测）在 main 上最近一次 `success`。
- [ ] **部署成功**：`deploy.yml` 在 main 上最近一次 `success`——生产 Worker 已经是
      待发布的那份代码。
- [ ] **验收全绿（不是"看起来绿"）**：手动 dispatch `acceptance.yml`，两项 live 检查都
      必须是 **PASS 而不是 SKIP**：
      - `Live tunnel checks (direct exit + mux)`（含直连、单 WS 并发、4 条 WS 分摊并发）
      - `CF-hosted target success rate`（PRD 要求 ≥ 99%）

      ⚠️ **两条容易骗过你的地方**：
      1. 没有 `NETMASTER_E2E_WORKER` / `PASSWORD` 时 live 项会整段 SKIP，跳过的 run 也是绿的；
      2. `Protocol-level checks` 这一步用 `node test/$t.mjs | tail -1` 收尾，**退出码会被管道
         吞掉**——`cron.mjs` 已删除、`proxyip`/`race` 需要 `cloudflare:*` shim，这些失败都
         只打印错误不改变结论。看这一步时要逐个套件确认有 `ALL PASS:` 字样。
- [ ] **中继池刷新工作流可用**：手动 dispatch `refresh-relays.yml`（可先 `dry_run`），
      最近一次 `success`。它是 Worker Cron 的替代品——池子空了，CF 托管目标就只剩内置
      兜底列表。
- [ ] **KV `id` 仍是占位符** `00000000000000000000000000000000`（32 个 0）。真实 id 由部署
      入口在部署时注入，入库文件里不该出现真值。
- [ ] **一次性诊断工作流已删**：`live-debug.yml`、`cron-debug.yml`（见第 6 节）。它们会把
      Worker 重新部署成 `DEBUG=1` 的调试形态，留在 main 上迟早误触发。
- [ ] **版本号确认**：见 `v0.2.0` 的取名理由（协议版本是"v2"，产品 tag 走 0.x 语义化版本）。

## 1. 合并 v2 → main

```bash
git checkout main
git pull
git merge --no-ff v2 -m "Merge v2: protocol rewrite (mux over WebSocket), Session/Router DO, ProxyIP exits"
git rev-list --count v2..main      # 应为 0
```

`--no-ff` 的理由：v2 上混着 M0 探针、临时诊断与若干 debug 提交，merge commit 给"v2 整体"
一个可以一次性 `git revert -m 1` 的锚点。

**确认**：

- [ ] `git log --oneline -1` 是那条 merge commit；
- [ ] `git rev-list --count v2..main` = 0。

## 2. 部署（合并后自动触发）

push 到 main 会触发 `deploy.yml`：构建 → 解析/创建 KV namespace 并把真 id 写进 runner 上的
`wrangler.toml` 副本 → `wrangler deploy`（DO migration 随部署自动应用）→ 透传 `PASSWORD`。

**确认**：

- [ ] `deploy.yml` 最近一次 `success`；
- [ ] 日志里 KV namespace 解析到了真实 id（不是 32 个 0）；
- [ ] 没有 `no PASSWORD secret set` 的 warning。

### 绑定域名（首次部署才需要）

- [ ] Cloudflare 控制台 → Workers → 该 Worker → Settings → Domains & Routes → 绑 custom
      domain。这一步同时建 DNS 记录和路由，**是客户端能连上的全部前提**；Worker 本身只
      部署到 `workers.dev`。

## 3. 打 tag

```bash
git tag -a v0.2.0 -m "v0.2.0 — protocol v2"
git push origin main
git push origin v0.2.0
```

`release.yml` 由 tag 触发，自动：`npm ci` → `node build.mjs` → 服务端测试（crypto/protocol/
integration/proxyip/race/router，带 `cloudflare:*` shim）→ 五平台 `go vet` → 五平台交叉编译
→ 产物自检（5 个二进制都 ≥ 1 MB）→ 客户端 `go test` → 生成 notes → 创建 Release。同 tag
重跑会先删旧 Release（`--cleanup-tag=false`，tag 本身不动）。

**确认**：

- [ ] `release` 工作流 `success`；
- [ ] Release 页面有 **7 个资产**：`_worker.js`、`wrangler.toml`、
      `netmaster-windows-amd64.exe`、`netmaster-linux-amd64`、`netmaster-linux-arm64`、
      `netmaster-darwin-amd64`、`netmaster-darwin-arm64`；
- [ ] Release notes 是英文、包含"部署要看 KV 占位符"的说明（见 `.assist/C5.md` 的
      notes 初稿；`release.yml` 里的模板若仍是中文，用
      `gh release edit v0.2.0 --notes-file <英文稿>` 覆盖）。

## 4. 部署后验证（打 tag 之后再做一遍）

- [ ] **acceptance 再手动 dispatch 一次**（合并后它的 `push: branches: [v2]` 触发器不再
      生效，必须手动跑），live 两项 PASS；
- [ ] **协议层验收**（可选）：dispatch `e2e.yml`，`worker` 填你的域名、`mode=live`；
- [ ] **中继池有数据**：`refresh-relays.yml` 最近一次 success，且 KV 里 `proxyip:top` 有值
      （用 `acceptance.yml` 的 `KV health snapshot` 步骤看，或
      `npx wrangler kv key get proxyip:top --namespace-id <id> --remote`）；
- [ ] **客户端实机**：任选一台机器跑 `netmaster serve`，日志出现
      `tunnel established via node <addr>`，浏览器能打开一个境外站点。服务端没有任何 HTTP
      端点，这一行就是"部署是否健康"最直接的回答。

## 5. 客户端分发

- [ ] 从 Release 下载对应平台的二进制；
- [ ] 写 `config.json`（放可执行文件旁边，或 `%AppData%/netmaster/config.json`）：

```json
{ "server": "<你的域名>", "password": "<PASSWORD>" }
```

- [ ] 运行 `netmaster serve`（Windows 双击 exe 等价；首次双击无配置会生成模板）；
- [ ] macOS 产物未签名，首次运行被 Gatekeeper 拦：
      `xattr -d com.apple.quarantine netmaster-darwin-arm64`；
- [ ] 退出时确认系统代理已还原（日志 `[sys] system proxy restored`）；被强杀时
      `netmaster restore`。

## 6. 出问题怎么办

- **Worker 回滚**：控制台 → Worker → Deployments → Rollback；或 `npx wrangler rollback`。
  只影响服务端，客户端不用动。
- **客户端回滚**：重新下载上一个 tag 的二进制替换（无自动更新）。
- **系统代理残留**：`netmaster restore`；Windows 的旧值备份在
  `%TEMP%/netmaster_sysproxy.json`。
- **口令泄露**：`npx wrangler secret put PASSWORD` 换新口令，所有客户端同步改
  `config.json`。两端不一致时表现为连不上（服务端只回 `0x01`，看不出原因）。
- **中继池空/全败**：`refresh-relays.yml` 的日志是验收依据（无论成败都留 artifact）；
  池子空时竞速退化为内置兜底列表，功能不残但 CF 托管目标会明显变差。

## 7. 合并后清理（一次性，2026-10-03 已完成，留档备查）

- [x] 删 `m0/` 与 `.github/workflows/m0-probe.yml`（M0 结论已固化在 `docs/m0-findings.md`），
      该文里的复核指引已同步改为"按实验描述重建探针"；
- [x] 删 `live-debug.yml`、`cron-debug.yml`（头部都写明"一次性诊断…结论落档后删除"）；
- [x] 删探针 worker `netmaster-m0`（`wrangler delete --name netmaster-m0 --force`）；
- [x] 撤销临时 SSH key（`gh api -X DELETE user/keys/<id>`）；
- [x] 修 `acceptance.yml` 的 `Protocol-level checks`：去掉已删除的 `cron`、给需要平台 shim 的
      套件加 `NODE_OPTIONS="--import ./test/shims/register.mjs"`、整步加 `set -o pipefail`；
- [x] 修 `docs/architecture.md` 的 Cron 章节（组件表、§7 整节）——Worker Cron 已删除，
      中继池刷新搬到 `refresh-relays.yml`；
- [x] 把 `refresh-relays.log` 这类本地产物加进 `.gitignore`。

## 附：两个版本号容易混

- **协议 v2**：帧格式的版本（`STREAM_ID | PAYLOAD`、HMAC 首帧、无版本协商）。仓库、分支、
  PRD 附录里到处在说它。
- **产品 tag `v0.2.0`**：发布版本。0.x 语义化版本下，破坏性变更升次版本号。两者不是一回事，
  Release notes 里把"protocol v2"写清楚即可。
