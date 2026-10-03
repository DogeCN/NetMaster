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
| DO 内并发出站连接 | 单 DO 12 条并发 `connect()` **全部成功**（M0 E2） | 6 槽竞速是预算控制，不是性能天花板 |
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
| 竞速槽位 / 单槽 / 全局 / 交错 | 6 / 1500 ms / 3000 ms / 120 ms | `race.js`（`RACE_*` 环境变量可覆盖） |
| KV 取前 N 个中继 | 4 | `race.js` `RACE_KV_TOP` |
| 中继健康记忆 TTL / 上限 | 10 分钟 / 256 条 | `proxyip.js` |
| Router DO 条目 TTL | 1 小时 | `router.js` `ROUTE_TTL_MS` |
| Router DO flush | 攒 5 秒或 50 条 | `router.js` |
| 中继池刷新周期 | 每小时左右（GH Actions `schedule`） | `refresh-relays.yml` |
| 并发隧道条数 / 空闲回收 | 4 条 / 45 秒 | `selector` `DefaultMuxTarget` / `idleTrimDelay`（条数可由 config.json 的 `tunnels` 改成 1–8） |
| 中继池探测：并发 / 超时 / 候选上限 | 8 / 3 秒 / 60 | `tools/refresh-relays.mjs` |
| 客户端入口候选上限 | 64 | `main.go` `maxEntries` |
| 客户端社区源等待上限 | 3 秒 | `main.go` `resolveEntries` |
| IP 优选：并发 / 单次超时 / 全流程预算 / 取前 N | 12 / 4 秒 / 10 秒 / 16 | `probe.go` |
| 规则集拉取：总预算 / 单源超时 / 单份上限 | 3 秒 / 3 秒 / 8 MiB | `rules/fetch.go` |
| 断线重连退避 | 1s → 2s → … → 32s，±20% 抖动 | `pending.go` `backoffFor` |
| 断线期间等待队列 | 上限 128 条、单条 10 秒 | `pending.go` |
| ECH 尝试预算 / 短路 | 2 秒 / 60 秒 | `client.go` / `ech.go` |
| 直连阻断冷却 | 30 分钟 | `selector.go` `directBlockedTTL` |
| geoip 表 TTL | 7 天 | `geoip.go` `DefaultTTL` |
| 中继池刷新周期 | 每小时左右（GH Actions `schedule`） | `refresh-relays.yml` |

## 存储配额

| 项 | 限额 | 我们的用量 |
|---|---|---|
| SQLite 行写（免费版） | 100k 行/日 | 主要来源是 Router DO flush：同一 target 只留最新一条（`RouteQueue` 按 hash 去重），个人规模无压力 |
| SQLite 行读（免费版） | 5M 行/日 | 每未命中流 1 读，会话级缓存摊薄 |
| SQLite 存储 | 免费版单 DO **1 GB**、账户总计 5 GB（10 GB 是付费版数字） | 只存目标哈希（不含域名），用量 < 1 MB |
| DO duration（GB-秒） | **免费计划有额度：13,000 GB-s/日**（付费 400,000 GB-s/月）；按 pricing 页系数 1 秒 DO 时间 = 0.128 GB-s ⇒ ≈ **28.2 DO·小时/天**。超额该类操作**硬失败**（免费版不是"超出计费"，是直接报错）。且"能休眠的空闲 DO 不计 duration" | 所以 pending timer 阻止 DO 休眠就等于烧额度。但服务端那个 185 秒判死定时器是 pending timer、会阻止 DO 休眠（m0 E4/E9），所以客户端在空闲期把多余隧道收掉、只留 1 条（`selector` 的 `idleTrimDelay`） |
| KV 读 | ~10 ms、最终一致 | 竞速读 `proxyip:top`（每未命中流一次，不在每条流的路径上）；写入侧是 GH Actions 每小时一次，不占 Worker 配额 |
| GH Actions 托管 runner | 私有仓库 2000 分钟/月（免费版），公开仓库免费 | 中继池刷新每轮约 1 分钟（含 checkout/setup-node），每小时一轮 ≈ 720 分钟/月 |

最后一行是这次把中继池刷新从 Worker Cron 搬到 GitHub Actions 换来的账：**约束方从
Cloudflare 免费版的 50 次/轮子请求，换成了 GitHub 的 runner 分钟数。** 前者是硬天花板
（所以要分批游标、要留 2 个给 KV 读写），后者对个人仓库宽裕得多——代价是触发时刻不再
准点（GH `schedule` 有几分钟级延迟），且仓库连续 60 天无活动时定时任务会被自动停用。

`target_hash` 只存目标域名（小写）SHA-256 前 16 字节十六进制，不存域名——路由表是缓存，
不是访问日志，没必要留可还原的目标名。

## 已知的能力边界

- **只支持 TCP。** 无 UDP 出站：QUIC 不会被代理，建议在浏览器里禁用 QUIC
  （`chrome://flags/#enable-quic`）强制回落 TCP；游戏、视频通话等 UDP 应用不在覆盖范围内。
  客户端 SOCKS5 对 UDP ASSOCIATE 直接返回不支持。
- **客户端不拦截 DNS。** 域名原样传给服务端，解析发生在边缘；DNS 查询若走系统解析器可能
  泄漏，是否配加密 DNS 由用户决定。
- **公共中继的出口 IP 被 CF 系站点拉黑是常态**，动态列表 + 竞速 + 亲和记忆是自愈机制不是
  根治。要根治只能自建中继（见 [relay.md](relay.md)）。
- **ECH 当前完全不生效**（2026-10-03 实测，非"部分网络"）：`utls` 在 ECH 模式下拿外层
  `cloudflare-ech.com` 的证书去匹配真实域名，必然失败；把 `ServerName` 改成 public_name
  则边缘直接回 `server rejected ECH`。失败被 `isStructuralECHFailure` 判成结构性故障 →
  熔断 60 秒 → 之后每次拨号都退回明文 SNI。
  客户端已改为**在启动日志里明说**（`[ech] ...VISIBLE SNI`），探测也不再被 ECH 拖垮
  （失败一次后本轮其余候选直接走明文，不再拿同一份配置一个个去撞）。
  修复方向：在 Cloudflare zone 上开启 Encrypted Client Hello；未开启前明文 SNI 是常态。
- **geoip 表只有 IPv4**，纯 IPv6 站点一律按"非 CN"处理（走代理）。
- **无版本协商**：协议升级是破坏性变更，客户端与服务端必须同时更新；错误版本的帧呈现为
  `0x01`，与密码错误不可区分。
- **PASSWORD 无长度或复杂度限制**，用户自负。协议层安全只保证 HMAC 签名与 TS 校验两项。
- **不做多用户、计费、审计**，不保证向后兼容。

## 合规

在 Workers 上运行通用代理游走在 Cloudflare 服务条款边缘，账号可能被封。应对：个人使用、
低流量；小号部署 + 账号隔离；绑定自定义域名；封禁不影响本地直连分流。本项目仅供个人技术
学习用途。
