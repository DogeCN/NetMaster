# v2 架构

本文描述 v2 分支的实际实现。PRD v1.1（[PRD.md](PRD.md)）是冻结基线，其与实现的
差异记在 PRD 文末的「附录：v2 实施修订记录」里；与 PRD 冲突处，**以本文和代码为准**。

## 1. 组件

| 组件 | 位置 | 职责 |
|---|---|---|
| Worker 入口 | `server/src/index.js` | 路径校验（固定 `/`）+ WebSocket 升级，转交 Session DO；非 WS 请求一律 404 空 body |
| Session DO | `server/src/session.js` | 每连接一个实例：首帧认证、流表管理、帧分发、出口选路、背压 |
| Router DO | `server/src/router.js` | 全局单实例（分片接口已预留）：目标 → 出口路径映射，SQLite + 异步批量 flush |
| KV | `wrangler.toml` 绑定 `KV` | 中继健康排名（`proxyip:top`，由 GitHub Actions 的 refresh-relays 每小时写入），last-good-wins |
| 客户端 | `client/` | SOCKS5/HTTP 入站、分流、入口优选、ECH 出站、mux |

### Session DO 的分配

Worker 用 `env.SESSION.idFromName(crypto.randomUUID())`——每条 WS 连接一个独立
Session DO，客户端不感知。DO 不落盘：状态只在实例内存与 WS 生命周期内有效。

### Hibernation

WS 经 `state.acceptWebSocket()` 注册，零流的空闲连接休眠、不驻留内存、不计 DO 时长。
客户端的协议层 Ping 由边缘自动应答，不唤醒 DO。

M0 实测补充了一条重要语义：**有出站 socket 的 DO 不会休眠**（挂起的 socket I/O 阻止
休眠），socket 关闭后才谈得上休眠。所以休眠时丢失的只可能是早已没有流量的流——客户端
在死流上再发数据会收到 `0x01` + CLOSE，自行回收。详见
[m0-findings.md](m0-findings.md) E4。

## 2. 跳数

| 路径 | 跳数 | 适用 |
|---|---|---|
| 客户端直连 | 2 | 规则判定直连，或出口层判定目标在中国大陆 |
| 经 Worker 直连出口 | 3 | 走代理、且目标**不由 Cloudflare 承载** |
| 经 Worker + ProxyIP 中继 | 4 | 走代理、且目标由 Cloudflare 承载 |

为什么第四跳躲不掉：Workers 的 `connect()` 不能拨 Cloudflare 自有网段（`server/src/exits.js`
的 `CF_V4` / `CF_V6` 提前拒绝，省掉一次注定失败的连接），而大量站点托管在 CF 上。此时
唯一出路是借第三方 HTTP CONNECT 中继：Worker `connect()` 到中继 →
`CONNECT host:port HTTP/1.1` → 隧道里跑的是客户端到目标的原始字节。

**NAT64 出口已经不存在了。** PRD §7.4 给它的价值正是绕开上述限制，但 M0 E3 实测
Workers `connect()` 对 IPv6 字面量与 NAT64 合成地址一律立即失败（<2ms，连拨号都没发生），
四个前缀、多个目标全部如此。这是运行时能力缺失，不是调参问题，已定稿砍掉。

## 3. 出口选路（Session DO）

每条逻辑流开流时按序尝试（`session.js` 的 `openExit`）：

```
① 直连 connect()                     成功即用（永远优先，不多付一跳）
   └ 失败
② 会话级内存缓存（egress Map）       命中且能连通 → 用（learn 在建立缓存的那条流上已做）
   └ 未命中 / 连通失败 → 删缓存
③ Router DO lookup(GET /lookup)      命中且未过 TTL → 用，并写回会话缓存
   └ 未命中 / 连通失败 → forget（立刻删，别让下一条流再踩同一脚）
④ 竞速 startRace()                   候选 ≤6，任一成功即用；learn 按类型分时机（见下）
   └ 全失败 → STATUS 0x03
```

**learn 的时机按中继类型分**（type 由 KV 条目一路带到竞速赢家）：

- `http-connect`：CONNECT 2xx 就是端到端验证，开流即 learn。
- `sni`：TCP 连通不构成验证（盲转发也能连），**隧道首字节到达才 learn**（`pumpOutbound`
  里的 `learnPending`）；3 秒首字节宽限超时则 forget 并丢弃。这是 Router DO 唯一的
  跨会话复用来源——没有它，每个新会话都要为同一目标重付一轮竞速的子请求预算。

要点：

- **直连失败本身就是信号**：`connect()` 拨 CF 网段必被平台拒，所以"直连失败"≈"目标在
  CF 网段或不可达"，这时才值得付中继那一跳。
- **没部署出口层就保持纯直连语义**：`ROUTER` 与 `KV` 绑定都缺席时，直连失败就是失败
  （`session.js` 里 `if (!this.env.ROUTER && !this.env.KV) return { error: direct.error }`）。
- **首字节宽限 3 秒**（`FIRST_BYTE_GRACE_MS`）：中继回了 200 只代表它愿意转发，不代表
  目标可达。学到映射后 3 秒内没有首字节，就承认学错了——删会话缓存 + `forget`。
- **Router DO 条目 TTL 1 小时**，与 refresh-relays 的刷新周期对齐；过期即视为未命中。
- **会话缓存上限 512 条**（`EGRESS_CACHE_MAX`），只为本连接内省一次查询，不是状态。

## 4. 协议 v2 帧格式

权威定义：`server/src/protocol.js` 与 `client/internal/proto/proto.go`（互为镜像，多字节
字段全部大端）。

| 帧 | 方向 | 格式 |
|---|---|---|
| 首帧 | C → S | `AUTH(16) \| TS(8) \| STREAM_ID(4) \| ATYP(1) \| ADDR \| PORT(2)` |
| 开帧 | C → S | `STREAM_ID(4) \| ATYP(1) \| ADDR \| PORT(2)` |
| 数据帧 | 双向 | `STREAM_ID(4) \| PAYLOAD`（PAYLOAD ≤ 64 KB） |
| 控制帧 | 双向 | `0x00000000 \| CTRL_TYPE(1) \| ...`（总长 ≤ 9 字节） |
| 响应帧 | S → C | `STREAM_ID(4) \| STATUS(1)` |

- `ATYP`：`0x01` IPv4、`0x02` 域名（1 字节长度前缀，小写）、`0x03` IPv6。
- `STREAM_ID ∈ [1, 0xFFFFFFFE]`；`0` 是控制帧专用前缀，`0xFFFFFFFF` 永久保留；递增到
  `0xFFFFFFFE` 后回绕到 1。
- 开帧与数据帧在裸字节上同形（都以 ID 开头），靠流状态机区分：客户端在收到该流的
  `STATUS 0x00` 之前不发任何数据帧，因此服务端只对"流表里不存在的 ID"按开帧解析。
  已关流的迟到数据帧靠 `closedIds` 静默丢弃，不会被误解析成开帧。
- **认证期间的帧要排队，不能丢**：HMAC 计算是 async（会让出执行权），客户端"发首帧后立刻
  并发开流"是常态（一个页面几十条连接），那些紧跟着首帧到达的开帧会看到 `authed` 仍为
  `false`。所以服务端在第一个 `await` 之前同步占位 `authPending`，后续帧进
  `pendingFrames` 队列等认证结果，认证成功后按到达顺序 `drainPending()` 处理。丢了它们，
  客户端只能等到 20 秒超时才知道自己失败了。队列有 1 MiB 上限
  （`PENDING_FRAME_LIMIT`），超了说明客户端不遵守协议，断开。

### 状态码

| STATUS | 含义 | 触发 |
|---|---|---|
| `0x00` | 成功 | — |
| `0x01` | 格式或认证错误 | HMAC 不符、TS 超窗、STREAM_ID 非法、ADDR 非法、流 ID 重复 |
| `0x02` | 目标禁连 | 私网/保留网段、Cloudflare 自有网段、端口 25 |
| `0x03` | 出口全部失败 | 直连失败且中继也全失败 |

控制帧 `0x01` = CLOSE：`0x00000000 \| 0x01 \| STREAM_ID(4)`，共 9 字节。

### 鉴权

```
AUTH = HMAC-SHA256(PASSWORD, TS ‖ STREAM_ID ‖ ATYP ‖ ADDR ‖ PORT)[:16]
```

- 连接级一次认证：只有首帧带 AUTH，后续开帧与数据帧零鉴权开销。
- 签名区是首帧中 AUTH 之后的全部字节（即 `frame.slice(16)`）。
- TS 为 Unix 秒，窗口 ±300 秒（`TS_WINDOW_SEC`）。
- 比较用常量时间函数：Workers 侧 `crypto.subtle.timingSafeEqual`，Node 侧退回逐字节
  异或累加（`crypto.js` 的 `safeEqualBytes`）。
- 口令不出网络，两端各自本地派生 16 字节签名。重放防护有意从轻：连接在 TLS 之内，
  能拿到明文帧的重放者本就能终止 TLS。
- 无版本协商：协议升级是破坏性变更，错误版本的帧呈现为 `0x01`，与密码错误不可区分。

### 保活与判死

- 客户端每 30 秒发一个 WebSocket **协议层** Ping（`mux.go` 的 `pingLoop`），边缘自动
  Pong 且不唤醒 DO；连续 2 个周期（65 秒）未收到 Pong 判死。
- 服务端只在醒着时判死：180 秒无任何数据帧则关闭会话。休眠期间不执行——休眠的前提是
  没有挂起的出站 socket，语义正好。
- 不做会话恢复：WS 断开时所有流一并关闭，上层应用自行重试。

### 背压

每流客户端→目标方向的缓冲上限 1 MiB（`STREAM_BUFFER_LIMIT`），超过即 CLOSE 该流，
不影响同一 WS 上的其他流。单条 WS 存在队头阻塞可能，缓冲上限保证阻塞只影响单条流。

## 5. 客户端结构

```
入站  SOCKS5(仅 CONNECT) / HTTP CONNECT       internal/proxy
分流  Clash 规则集 → geoip → 兜底             internal/rules, internal/geoip
出口  节点池、直连/代理自适应、重连            internal/selector
mux   单 WS 多流，按 STREAM_ID 分发            internal/outbound/mux.go
传输  TCP + TLS(ECH) + WebSocket              internal/tlsutil, internal/outbound/client.go
入口  域名 DNS 解析 + 社区优选源 + 延迟优选     internal/entry + selector.Optimize
```

- **入口候选两个来源**：服务端域名自身的 DNS 解析（永远可用的兜底）+ 三个社区优选源
  并行拉取。每次启动都尝试刷新，社区源有界等待 3 秒，全挂退磁盘缓存；候选总数上限 64
  （`maxEntries`）。
- **IP 优选**（`selector.Optimize`）：对候选并发测延迟，取最快的 16 个进池。全流程 ≤ 10 秒，
  超预算就用已到手的结果——优选是优化，不是能不能用的前提。启动日志：
  `[probe] <N> entries -> <M> nodes in <耗时>`。
- **分流**：并行拉四份内置 Clash 规则集（预算 3 秒），部分源挂了用缓存补，全挂退内置兜底集。
  详见 [routing.md](routing.md)。
- **传输**：优先 ECH（真实 SNI 加密，外层是 `cloudflare-ech.com`），ECH 有 2 秒尝试预算
  （`echAttemptBudget`），超预算或结构性失败则退普通 TLS，并进入 60 秒短路（`echDownTTL`），
  避免一批拨号同时踩坑。ECH 模式下直连优选 IP 而非域名——域名直连会让系统 DNS 解析出多个
  IP，其中不少并不承载目标域名，SYN 超时重传把握手拖到秒级。
- **首次连通验证**：serve 启动后在后台做一次真实的 TLS+WS+首帧认证建流（目标
  `www.google.com:443`——平台禁拨 80、example.com 已迁 CF，见 m0-findings.md E6/E7），
  结果补一行 `tunnel established via node <addr>`。

## 6. 可观测性

**没有任何 HTTP 诊断端点**——没有 `/health`、没有 `/stats`、没有 `/sub`。非 WS 请求
一律 404 空 body 且无额外头（避免指纹）。部署是否健康，由"客户端能不能连上"直接回答。

- 服务端：设 Worker 变量 `DEBUG=1`，`[session]` / `[router]` 前缀的日志会打开，用
  `npx wrangler tail` 实时查看。注意 M0 E9：**DO 内的 console 输出在 tail 上不可见**，
  这些日志只有 worker 入口层的粗筛价值；"某件事到底有没有发生"要查 KV（`debug:lastExit`）。
- 客户端：标准输出带时间戳的日志（`log.Ltime`），关键事件包括
  `entries: N (community: net|cache|none)`、`[probe] N entries -> M nodes in …`、
  `rules: N entries (fetched|cache|builtin, skipped K lines)`、
  `[geoip] CN ranges ready: N (fetched|cache)`、`[sys] system proxy ON -> …`、
  `tunnel established via node …`、
  `[route] <host> direct unusable (…) — switched to proxy and replayed`。

## 7. 中继健康检查（仓库侧）

Worker Cron 已移除（原因与替代方案见 PRD 附录 A6）。候选探测、排序与写入 `proxyip:top`
由 `.github/workflows/refresh-relays.yml` 在 GitHub Actions 上每小时跑一次（可手动
dispatch），主体在 `server/tools/refresh-relays.mjs`：

1. **候选**：拉 IPDB bestproxy 等外部源 + 内置兜底（CMLiussss 后备列表）；`filterSelf`
   剔除回指本 Worker 的条目。
2. **并发探测**：TLS 握手探测（`tls.connect`，`servername=www.cloudflare.com`）区分
   真正的 SNI 路由中继与盲转发器/失效节点——M0 之后 ProxyIP 只用 SNI 型中继。
3. **排序**：成功率 desc → 平均延迟 asc（runner 每轮全新观测，没有可平滑的历史，
   不做 EWMA），取前 4 写进 `proxyip:top`。
4. **last-good-wins**：本轮全败不写、保留上一轮数据、退出码 0。

排障现场是 Actions 的 run 页面，不是 `wrangler tail`；`cron:lastRun` / `cron:cursor` /
`cron:pending` 三个 KV 键已不存在。

## 8. 已移除的东西

写下来是为了别再找它们：

- **NAT64 出口**：平台不支持 IPv6 出站（M0 E3）。
- **Worker Cron 健康检查**：免费版触发不可靠且 50 子请求/轮逼出分批续跑；搬去
  GitHub Actions（见上节与 PRD 附录 A6）。`cron.js`、`scheduled` handler、
  `[triggers]` 已删。
- **服务端关系型数据库**：已删除。中继亲和映射改用 Router DO 的 SQLite，部署不再需要建库
  与建表步骤。
- **`schema.sql`**：随上面那条一起消失。
- **现成代理协议与用户标识符**：协议是自研的，口令两端本地派生，没有需要用户生成或
  保管的标识符，也不兼容任何现成代理协议。
