# 限制与配额

本文以 M0 实测（[m0-findings.md](m0-findings.md)，2026-10-02）与 v2 代码常量为准。平台
限额可能调整，部署前以 developers.cloudflare.com 当前页面为准；标注"实测"的条目是
2026-10-02 在本账号上跑出来的，不是引用文档。

## 平台限制（实测）

| 限制 | 实测结论 | 应对 |
|---|---|---|
| 每日请求（免费版 10 万） | **WS 消息按 ≈1:1 计入请求**，未观察到任何 20:1 折算（M0 E1：整点桶 829 requests，若 20:1 成立应在 ~75） | 协议层吝啬帧数，见下节 |
| `connect()` IPv6 出站 | **不支持**。IPv6 字面量与 NAT64 合成地址一律 <2ms 立即失败，连拨号都没发生；四个前缀（level66 / well-known / Trex / nat64.net）全部如此，IPv4 对照组 4ms 成功（M0 E3） | NAT64 出口已从架构移除；CF 承载目标只剩 ProxyIP 中继 |
| `connect()` 禁连目标 | 私网/保留网段、Cloudflare 自有网段、端口 25；无 UDP 出站；禁回连自身 | `exits.js` 提前拒绝（省一次注定失败的连接），回 `0x02`；客户端私网强制直连 |
| DO 内并发出站连接 | 单 DO 12 条并发 `connect()` **全部成功**（M0 E2） | 顺序拨号后并发很少见；这 12 条是平台硬顶 |
| DO 休眠与出站 socket | **有出站 socket 的 DO 不休眠**（挂起 I/O 阻止休眠）；socket 关闭后才可休眠；空闲 socket 由对端在数十秒内关闭（M0 E4） | "休眠期间流保活"不成立也无需成立：零流空闲 WS 走 Hibernation 不驻留内存；每流死亡走 CLOSE |
| 脚本体积 | 1 MB（压缩）。当前产物 `_worker.js` 约 51 KB（未压缩，含注释） | 余量充足 |
| DO WS 接收消息 | 32 MiB | 单帧 ≤ 64 KB（主动设计约束，不随平台变化） |
| 系统代理不转发 UDP | 平台限制 | 客户端文档提示禁用 QUIC |

## 请求数：最重要的那一条

计费口径（A8 更正）：pricing 页脚注写明 **20:1 折算只作用于 billing**，DO 的 analytics/指标反映实际用量 —— 所以在 analytics 桶里测不到折算是必然的，不能据此断言"平台没有折扣"（E1 措辞已更正）。**限额按 billing 单位还是按实际请求执行，官方页未写明**。无论哪种口径，应对不变：数据帧尽量满 64KB、心跳只走 WS 协议层 Ping。

协议层的对策（都已实现）：

- **数据帧尽量满帧**，单帧 PAYLOAD 上限 64 KB（`MAX_PAYLOAD`）。小包逐帧发是最糟的形态。
- **心跳只走 WebSocket 协议层 Ping**：客户端每 30 秒一个 Ping，边缘自动 Pong，不产生 DO
  消息、不唤醒 DO，因此**不计请求**（M0 结论）。
- **没有逐帧 ACK**，没有应用层心跳控制帧（`CTRL_TYPE 0x00` 保留但未使用）。
- **连接级一次认证**：只有首帧带 AUTH，后续开帧零鉴权开销——不是省消息，是省掉每流一次
  往返。

个人规模（每天数千到数万帧）在 10 万/日之内。但"mux 摊薄请求量"的叙事不成立，别按那个
假设设计新功能。

## 协议与实现常量

| 项 | 值 | 位置 |
|---|---|---|
| 单帧 PAYLOAD 上限 | 64 KB | `protocol.js` / `proto.go` |
| 每流背压上限 | 1 MiB | `session.js` `STREAM_BUFFER_LIMIT` |
| 认证期间帧队列上限 | 1 MiB | `session.js` `PENDING_FRAME_LIMIT` |
| 流 ID 区间 | `[1, 0xFFFFFFFE]`，`0` 控制帧专用，`0xFFFFFFFF` 保留 | `protocol.js` |
| 控制帧总长上限 | 9 字节 | `protocol.js` |
| 鉴权 TS 窗口 | ±300 秒 | `crypto.js` `TS_WINDOW_SEC` |
| 客户端 Ping 周期 / 判死 | 30 秒 / 连续 2 个周期（65 秒）无 Pong | `mux.go` `pingLoop` |
| 服务端空闲判死 | 180 秒 | `session.js` `touch()` |
| 出口直连超时 | 15 秒 | `exits.js` `CONNECT_TIMEOUT_MS` |
| 首字节宽限 | 3 秒 | `session.js` `FIRST_BYTE_GRACE_MS` |
| 会话出口缓存上限 | 512 条 | `session.js` `EGRESS_CACHE_MAX` |
| 顺序拨号：候选上限 / 单条尝试 | 6 / 5000 ms | `order.js`（`ORDER_*` 环境变量可覆盖） |
| 中继顺序写回 KV 预算 | 每会话 3 次 | `session.js` `RELAY_ORDER_WRITE_BUDGET` |
| 子请求预算 / 回收 | 20 / 排空在途流后回收 | `session.js` `CONNECT_BUDGET` |
| 并发隧道条数 / 空闲回收 | 4 条 / 45 秒 | `selector` `DefaultMuxTarget` / `idleTrimDelay`（条数可由 config 的 `tunnels` 改成 1–8） |
| 中继部署测速：并发 / 单次超时 / 尝试次数 / 候选上限 / 取前 N | 4 / 6 秒 / 2 / 60（工作流 RELAYS 全量）/ 6 | `tools/probe-relays.mjs` |
| 客户端入口候选上限 | 64 | `main.go` `maxEntries` |
| 客户端优选缓存刷新判据 | 缺失 / 超 24h / 少于 12 条 | `main.go` `entryCacheTTL` / `minCachedEntries` |
| 客户端社区源等待上限 | 3 秒 | `main.go` `resolveEntries` |
| IP 优选：并发 / 单次超时 / 全流程预算 / 取前 N | 12 / 4 秒 / 10 秒 / 16 | `probe.go` |
| 规则集拉取：总预算 / 单源超时 / 单份上限 | 3 秒 / 3 秒 / 8 MiB | `rules/fetch.go` |
| 断线重连退避 | 1s → 2s → … → 32s，±20% 抖动 | `pending.go` `backoffFor` |
| 断线期间等待队列 | 上限 128 条、单条 10 秒 | `pending.go` |
| ECH 尝试预算 / 短路 | 2 秒 / 60 秒 | `client.go` / `ech.go`（拒绝时 RetryConfigList 自愈，先于短路） |
| 直连阻断冷却 | 30 分钟 | `selector.go` `directBlockedTTL` |
| 直连 TCP 失败负记忆 | 5 分钟 | `selector.go` `directDownTTL` |
| 分片直连记忆 | 6 小时 | `selector.go` `fragDirectTTL` |
| geoip 表 TTL | 7 天 | `geoip.go` `DefaultTTL` |

## 存储配额

| 项 | 限额 | 我们的用量 |
|---|---|---|
| SQLite 行写（免费版） | 100k 行/日 | Router DO 已移除；当前无 SQLite 写入方 |
| SQLite 行读（免费版） | 5M 行/日 | 每未命中流 1 读，会话级缓存摊薄 |
| SQLite 存储 | 免费版单 DO **1 GB**、账户总计 5 GB（10 GB 是付费版数字） | 只存目标哈希（不含域名），用量 < 1 MB |
| DO duration（GB-秒） | **免费计划有额度：13,000 GB-s/日**（付费 400,000 GB-s/月）；按 pricing 页系数 1 秒 DO 时间 = 0.128 GB-s ⇒ ≈ **28.2 DO·小时/天**。超额该类操作**硬失败**（免费版不是"超出计费"，是直接报错）。且"能休眠的空闲 DO 不计 duration" | 所以 pending timer 阻止 DO 休眠就等于烧额度。但服务端那个 185 秒判死定时器是 pending timer、会阻止 DO 休眠（m0 E4/E9），所以客户端在空闲期把多余隧道收掉、只留 1 条（`selector` 的 `idleTrimDelay`） |
| KV 读 | ~10 ms、最终一致 | 会话首次需要中继时读 `proxyip:top` 一次（随后走内存）；运行期顺序重排会写回（每会话 3 次预算）；部署时测速全量覆写 |
| GH Actions 托管 runner | 私有仓库 2000 分钟/月（免费版），公开仓库免费 | 部署测速每轮约 1 分钟，随部署频率走（无定时任务） |

中继候选的写入方从"Worker Cron → GH Actions 每小时任务"一路演进到"部署时测速"：约束方
从 Cloudflare 的 50 子请求/轮，变成 runner 分钟数，再变成"部署频率"——个人部署一天几次，
KV 写配额（1000/日）从此只服务运行期的顺序重排（每会话 3 次）与 profile 度量。

KV 写入曾经被打爆过一次（2026-10-06）：每小时 refresh-relays + 服务端运行时写入叠加。
此后写入方只剩部署测速（每次 ≤1 写）与运行期重排（每会话 3 次预算），profile 落盘每会话
3 次。

## 已知的能力边界

- **只支持 TCP。** 无 UDP 出站：QUIC 不会被代理，建议在浏览器里禁用 QUIC
  （`chrome://flags/#enable-quic`）强制回落 TCP；游戏、视频通话等 UDP 应用不在覆盖范围内。
  客户端 SOCKS5 对 UDP ASSOCIATE 直接返回不支持。
- **客户端不拦截 DNS。** 域名原样传给服务端，解析发生在边缘；DNS 查询若走系统解析器可能
  泄漏，是否配加密 DNS 由用户决定。
- **公共中继的出口 IP 被 CF 系站点拉黑是常态**，部署测速 + 顺序记忆 + 会话亲和是自愈
  机制不是根治。要根治只能自建中继（见 [relay.md](relay.md)）。
- **ECH 生效（2026-10-04 实测），并且它是必需品不是锦上添花**：`echprobe` 4/4 内层证书
  校验通过；同一边缘 IP 上明文 SNI 写本域 4/4 被 RST（换 `www.cloudflare.com` 同 IP 200，
  即 IP 本身可达）。所以 ECH 失败后熔断 60 秒、退明文 SNI 的那条兜底路径**对本域表现为连
  不上**——"明文 SNI 是常态"这句在 10-03 写下时成立，现在反过来了。
  客户端在启动日志里明说当前是哪一态（`no ECH config published` / `...VISIBLE SNI` /
  `SNI hidden by ECH`）；握手级复核用 `go run ./cmd/echprobe <域名>`（打印取配置、
  带校验的 ECH 握手、不校验握手、明文对照四步）。配置过期导致的握手失败（边缘回
  `server rejected ECH`）已带自愈：客户端从拒绝里取出服务端下发的 RetryConfigList、
  热更新缓存并原地重试一次，不再因此熔断 60 秒。探测不再被 ECH 拖垮这一点不变
  （失败一次后本轮其余候选直接走明文，不再拿同一份配置一个个去撞）。
  历史：2026-10-03 的记录是"完全不生效"（外层证书不匹配 / `server rejected ECH`），
  10-04 未能复现，归因为当时本机网络波动（未进一步验证）。
- **geoip 表只有 IPv4**，纯 IPv6 站点一律按"非 CN"处理（走代理）。
- **无版本协商**：协议升级是破坏性变更，客户端与服务端必须同时更新；错误版本的帧呈现为
  `0x01`，与密码错误不可区分。
- **PASSWORD 无长度或复杂度限制**，用户自负。协议层安全只保证 HMAC 签名与 TS 校验两项。
- **不做多用户、计费、审计**，不保证向后兼容。

## 合规

在 Workers 上运行通用代理游走在 Cloudflare 服务条款边缘，账号可能被封。应对：个人使用、
低流量；小号部署 + 账号隔离；绑定自定义域名；封禁不影响本地直连分流。本项目仅供个人技术
学习用途。
