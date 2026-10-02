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

1:1 计费的含义是：**多路复用省下的只是 WS 建连成本，每帧仍是一条请求。**

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
| Router DO 条目 TTL | 1 小时（与 Cron 周期对齐） | `router.js` `ROUTE_TTL_MS` |
| Router DO flush | 攒 5 秒或 50 条 | `router.js` |
| Cron 子请求预算 | 48 次/轮（免费版上限 50） | `cron.js` `SUBREQUEST_BUDGET` |
| Cron 单次探测超时 | 3 秒 | `cron.js` `PROBE_TIMEOUT_MS` |
| Cron 评分 | EWMA α=0.3，延迟:成功率 = 7:3 | `cron.js` |
| 客户端入口候选上限 | 64 | `main.go` `maxEntries` |
| 客户端社区源等待上限 | 3 秒 | `main.go` `resolveEntries` |
| IP 优选：并发 / 单次超时 / 全流程预算 / 取前 N | 12 / 4 秒 / 10 秒 / 16 | `probe.go` |
| 规则集拉取：总预算 / 单源超时 / 单份上限 | 3 秒 / 3 秒 / 8 MiB | `rules/fetch.go` |
| 断线重连退避 | 1s → 2s → … → 32s，±20% 抖动 | `pending.go` `backoffFor` |
| 断线期间等待队列 | 上限 128 条、单条 10 秒 | `pending.go` |
| ECH 尝试预算 / 短路 | 2 秒 / 60 秒 | `client.go` / `ech.go` |
| 直连阻断冷却 | 30 分钟 | `selector.go` `directBlockedTTL` |
| geoip 表 TTL | 7 天 | `geoip.go` `DefaultTTL` |
| Cron 周期 | 每小时（`0 * * * *`） | `wrangler.toml` |

## 存储配额

| 项 | 限额 | 我们的用量 |
|---|---|---|
| SQLite 行写（免费版） | 100k 行/日 | 主要来源是 Router DO flush：同一 target 只留最新一条（`RouteQueue` 按 hash 去重），个人规模无压力 |
| SQLite 行读（免费版） | 5M 行/日 | 每未命中流 1 读，会话级缓存摊薄 |
| SQLite 存储 | 单 DO 10 GB；账户总计免费版 5 GB | 只存目标哈希（不含域名），用量 < 1 MB |
| KV 读 | ~10 ms、最终一致 | 竞速读 `proxyip:top`；Cron 读写 `cron:*` 游标键。都不在每条流的路径上 |

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
- **ECH 在部分网络下间歇失败**（服务端返回 outer 名证书而 utls 用 outer 名校验 hostname）。
  客户端有 2 秒预算与 60 秒短路兜底，但该网络下明文 SNI 会暴露。
- **geoip 表只有 IPv4**，纯 IPv6 站点一律按"非 CN"处理（走代理）。
- **无版本协商**：协议升级是破坏性变更，客户端与服务端必须同时更新；错误版本的帧呈现为
  `0x01`，与密码错误不可区分。
- **PASSWORD 无长度或复杂度限制**，用户自负。协议层安全只保证 HMAC 签名与 TS 校验两项。
- **不做多用户、计费、审计**，不保证向后兼容。

## 合规

在 Workers 上运行通用代理游走在 Cloudflare 服务条款边缘，账号可能被封。应对：个人使用、
低流量；小号部署 + 账号隔离；绑定自定义域名；封禁不影响本地直连分流。本项目仅供个人技术
学习用途。
