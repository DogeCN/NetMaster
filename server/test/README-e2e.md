# e2e — 真实部署验收脚本

`test/e2e.mjs` 是**协议层**的端到端验收：它自己实现协议 v2 客户端（首帧 HMAC
认证、开帧、数据帧、响应帧、CLOSE 控制帧），用 `ws` 连上**已部署的** Worker，
逐项核对 PRD §14 M1-M5 的量化验收标准。

它与 `.github/workflows/acceptance.yml` 里的 Go 客户端验收互补：那边验的是"用户
实际会走的路"（TLS/ECH → WS → Session DO → 出口 → 回程），这边验的是帧与状态码
本身。所有判定都要求**两个以上独立证据**（例如 0x02 之后必须还有一条同 id 的
CLOSE 控制帧），拿不到证据就 SKIP 并写清原因。

## 前置条件

| 场景 | 需要什么 |
|---|---|
| 本地自检 | Node ≥ 20，`npm ci`（用仓库自带的 `ws`）。**不需要网络。** |
| 真实部署 | 上面全部 + 一条**能直连 Cloudflare 边缘的干净网络**（GitHub Actions runner 可以；本机被 DNS 污染 / SNI 阻断时跑不了）+ `NETMASTER_PASSWORD` |
| 第 7 项（KV） | 再加 `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID`（KV 内容只能经 Cloudflare REST API 读，没有别的接口） |

口令只从环境变量读。**不要**把它写在命令行（会进 shell 历史 / CI 日志），也不要
写进任何文件——它是 Worker secret，仓库根的 `.env` 由部署侧持有，本脚本不碰。

## 怎么跑

```bash
cd server
npm ci

# 1) 本地自检（进程内拉起 devserver.mjs + 本地 HTTP 目标）
#    跑第 1-4 项证明脚本逻辑正确；第 5-8 项如实 SKIP。
node test/e2e.mjs
echo "exit=$?"

# 2) 真实部署：全部 8 项
export NETMASTER_ENDPOINT=wss://netmaster.<account>.workers.dev/
export NETMASTER_PASSWORD='<worker secret>'
node test/e2e.mjs
echo "exit=$?"

# 3) 只跑其中几项 / 调整规模
node test/e2e.mjs --only=1,2,3
node test/e2e.mjs --streams=200 --rounds=100 --idle=600
```

`NETMASTER_ENDPOINT` 可以只写主机名（`netmaster.<account>.workers.dev`），脚本补
`wss://` 并把路径固定成 `/`（`src/index.js` 只在根路径上做 WS 升级）。

GitHub Actions 里（口令与 token 走 secrets）：

```yaml
- name: Protocol-level acceptance
  working-directory: server
  env:
    NETMASTER_ENDPOINT: ${{ vars.NETMASTER_WORKER }}
    NETMASTER_PASSWORD: ${{ secrets.PASSWORD }}
    CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
    CLOUDFLARE_ACCOUNT_ID: ${{ secrets.CLOUDFLARE_ACCOUNT_ID }}
  run: |
    npm ci
    node test/e2e.mjs --rounds=20
```

### 环境变量与开关

| 名称 | 默认 | 说明 |
|---|---|---|
| `NETMASTER_ENDPOINT` / `--endpoint` | —（未给则本地模式） | 已部署 Worker 的 `wss://` 地址或裸主机名 |
| `NETMASTER_PASSWORD` | — | **live 模式必需**，缺失直接退出码 2 |
| `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID` | — | 第 7 项读 KV 用；缺失则该项 SKIP |
| `NETMASTER_KV_ID` | 自动按 `title=netmaster` 解析 | 钉死 KV namespace id（与 `deploy.yml` 同口径） |
| `E2E_DIRECT_TARGET` / `--direct` | `example.com:80` | 第 3、4 项的明文 HTTP 目标，**必须是非 CF 托管**的（走 `connect()` 直连） |
| `E2E_CF_TARGET` / `--cf` | `neverssl.com:80` | 第 5、6 项的 **CF 托管**明文 HTTP 目标（直连必被平台拒，只能走 ProxyIP 竞速） |
| `E2E_CF_ROUNDS` / `--rounds` | `20` | 第 5 项的轮数 |
| `E2E_CF_MIN_RATE` / `--minrate` | `99` | 成功率门槛（百分数） |
| `E2E_MUX_STREAMS` / `--streams` | `100` | 第 4 项的并发流数 |
| `E2E_IDLE_SECONDS` / `--idle` | live 300 / local 12 | 第 8 项静置时长 |
| `E2E_PING_SECONDS` / `--ping` | live 30 / local 3 | 第 8 项心跳间隔 |
| `E2E_STREAM_TIMEOUT_MS` | `25000` | 等响应帧的上限（生产直连 15s + 竞速 3s，留余量） |
| `E2E_SMTP_TARGET` | `example.com:25` | 第 2.3 项的端口 25 目标 |
| `E2E_ONLY` / `--only` | 全跑 | 只跑编号前缀匹配的项，如 `1,2` 或 `5.1` |
| `E2E_MODE` / `--mode` | 自动（有 endpoint 即 live） | `live` / `local` |

### 退出码

- `0`：无 FAIL
- `1`：至少一项 FAIL
- `2`：配置错误（live 模式缺 `NETMASTER_PASSWORD`、`--mode` 非法）

## 验收项与 PRD 对应

每项输出 `PASS` / `FAIL` / `SKIP(原因)`，结尾一行 `E2E: N pass, M fail, K skip`。

| # | 检查 | 判据 | 对应 |
|---|---|---|---|
| 1.1 | 错误口令的首帧 | 响应帧 `0x01` **且**收到连接关闭 | M1「认证失败返回 0x01 静默关闭」、PRD §5 |
| 1.2 / 1.3 | TS 偏移 −400s / +400s | 同上（窗口 ±300s，`crypto.js` `TS_WINDOW_SEC`） | PRD §5、§4.4 |
| 1.4 | 首帧 `STREAM_ID = 0` | `0x01`（id=0）**且**连接关闭 | PRD §4 流 ID 规则 |
| 1.5 | 开帧 `STREAM_ID = 0xFFFFFFFF` | `0x01` | 同上（0xFFFFFFFF 永久保留） |
| 1.6 | 开帧 `ATYP = 9` | `0x01` | PRD §4 地址编码 |
| 2.1 | 私网 `10.0.0.1:80` | `0x02` + 同 id 的 CLOSE 控制帧 | PRD §4.4、`exits.js` `PRIVATE_V4` |
| 2.2 | TEST-NET-1 `192.0.2.1:80` | `0x02` + CLOSE | 同上（192.0.2.0/24 在保留网段清单里，所以是 0x02 而不是 0x03） |
| 2.3 | 端口 25 | `0x02` + CLOSE | `exits.js` `isForbidden` 首条判定 |
| 3.1 | 直连出口 `example.com:80` | 首帧开流 `0x00` → HTTP `200` + 非空 body | **M1「curl 经代理访问 HTTP 成功」**：整条链路 + Session DO + `connect()` 出站 |
| 4.1 | 一条 WS 上并发 100 条流 | 100 条全 `0x00`，且 100 个请求各自拿到 HTTP 200 | **M2「单 WS 并发 ≥ 100 条流」** |
| 5.1 | CF 托管目标 N 轮 | 成功率 ≥ 99%，打印百分比与失败原因分类 | **M3「ProxyIP 成功率 ≥ 99%」** |
| 6.1 | 同一目标连 3 次（每次新连接） | 第 2/3 轮不再出现 `0x03`，并打印建流耗时 | M3「映射命中后建流更快 / 缓存复用」 |
| 7.1 | KV `proxyip:top` + `cron:lastRun` | key 非空且能解析出中继；打印内容与 lastRun 距今分钟数 | **M5「评分写入 KV 且下一周期可读到」** |
| 8.1 | 空闲存活 | 只走 WS 协议层 Ping 静置 5 分钟，连接未断、Pong 持续回来 | **M2「空闲 5 分钟会话存活」** |

## 判定口径与已知局限

这些是脚本刻意不做的事，读结果前先知道：

1. **第 6 项拿不到 DO 内部状态。** Durable Object 的 SQLite 存储没有任何公开
   Cloudflare API 可以读，脚本只能报告黑盒现象（第 2/3 轮有无 `0x03`、建流
   耗时对比）并明确写出来，不伪造"命中了 Router DO"的结论。另外每轮都换新
   连接是刻意的：同一连接里第 2/3 轮命中的是 Session DO 的会话级内存缓存
   （`session.js` 的 `this.egress`），那样证明不了 Router DO；两轮之间等 6s
   让 Router DO 的 +5s Alarm 把映射 flush 落盘。
2. **第 8 项可能撞上 185s 的空闲关闭。** `session.js` 的 `touch()` 在实例醒着
   时会起一个 180s 定时器，到点关连接，而 **WS 协议层 Ping 不经过
   `webSocketMessage`、不会重置这个定时器**。Hibernation 会把定时器一起丢掉，
   所以真实行为取决于 DO 是否处于醒着状态。若该项 FAIL，脚本会打印连接是在
   第几秒断的，以及关闭码，用来区分"边缘空闲回收"和"DO 主动关"。
3. **DO 用量不查。** 免费版的 DO/SQLite 时长与行数没有逐实例的公开查询接口，
   第 8 项只证明连接没被断开，不输出任何配额数字。
4. **≥99% 与轮数。** 默认 20 轮时，"≥99%" 实际上要求 20/20 全中。要让 1 次
   失败仍能达标，把 `--rounds` 提到 100 以上再判。
5. **第 5 项的失败分类**：`0x03`=六个槽位竞速全败、HTTP 403/401=隧道通了但出口
   被目标拒、其余按超时 / 建流状态码分类计数，逐条打印，不会合并成一个"失败"。
6. **首帧 + 非法 ATYP 只会静默关闭。** `session.js` 在 `parseFirstFrame` 返回
   null 时不回复 STATUS（结构上还没有可回的流 id），所以 1.6 走开帧路径验 ATYP。
7. **本地模式的第 2.3 项用域名而不是本地目标。** `devserver.mjs` 的
   `denyLoopback=false` 会对 `127.0.0.1` 整条跳过禁连检查（那是给测试开回环的
   开关），而生产是按 `port === 25` 最先判；用域名才能验到同一条代码路径。
8. **本地模式不是部署验收。** 它的 PASS 只证明脚本逻辑本身（第 1-4 项），
   输出里会明确标注。