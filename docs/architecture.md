# 架构

## 两端的职责

**客户端（Go，`client/`）** 跑在用户机器上，职责是"决定这个请求该从哪儿出去"：

- 提供 HTTP / SOCKS5 代理给浏览器
- 分流：显式规则 + IP 归属 + 域名亲和
- 入口候选池：多来源、多入口自适应、故障转移、延迟探测
- ECH 出站、SNI 隐藏
- 接管 / 还原系统代理，看门狗兜底
- 学到的状态（域名亲和、直连冷却）落盘，重启不丢

**服务端（JS，`server/`）** 跑在 Cloudflare Worker 上，职责是"最后一跳"：

- 终止内部协议 over WebSocket，解 mux 帧
- 出站连接目标；目标是 Cloudflare 承载时改经中继
- 维护域名 → 中继的亲和（并持久化到 D1）
- 中继列表从社区源动态获取

两端各只有一个凭据：**PASSWORD**。GitHub Secrets 配置，CI 透传成 Worker Secret；
客户端拿同一个口令本地派生。网络上只出现派生值（16 字节 auth），在 TLS 之内。

## 协议

两端都是我们的，所以协议里没有任何为兼容付出的字节（见 `server/src/protocol.js`
与 `client/internal/outbound`，两边的实现一一对应）：

```
一条 WebSocket（路径 /），全部为二进制消息。

  消息 1（握手）  16 字节 auth = md5(utf8(PASSWORD))
                  不匹配 → close(1008)。鉴权是连接级的，会话级零开销。
  会话开帧        mux 帧，payload = [port u16 BE][addrType][addr][初始数据]
                  addrType 1 = IPv4(4B)，2 = 域名(1B 长度 + 字节)，3 = IPv6(16B)
  数据帧          mux 帧，payload = 原始字节
  控制帧          [0x00][idLen][sessionId][utf-8 文本]
                  空文本 = 会话就绪；非空 = 失败原因（客户端把真实原因还给调用方）
  mux 帧          [idLen][sessionId][payload]，一条 WS 上并发跑多个会话
```

设计取舍：

- **mux 是强制的**。一条 WS 跑多个会话，省掉每请求的 TLS+WS 握手。
- **没有响应头**。客户端从"会话首帧到达"知道链路已通，不需要 VLESS 那个
  2 字节 respHeader；但"上游连接失败"必须显式告知（控制帧），否则客户端只能
  乐观返回 200，浏览器会把连接错误误报成证书错误。
- **没有 HTTP 端点**。没有 /health、/sub、/relaycheck、/probe。部署是否健康由
  "客户端能不能连上"直接回答，不需要一个专门回答这个问题的页面。

## 入口候选：服务端域名 + 社区源，客户端管顺序

入口 = 客户端拨的边缘地址。候选集两个来源（`internal/entry`）：

1. **服务端域名自身的 DNS 解析** —— 唯一不依赖第三方的源，只要域名能解析就有候选。
2. **社区优选 IP 源** —— 内置多个（ipdb / CloudFlareYes / bestcf），每次启动并行
   拉取、逐源容错；全挂退磁盘缓存。硬编的是"从哪拿列表"而不是列表本身，列表
   会过期，来源不会。

自然会产生一个疑问：**既然入口 IP 快慢只有客户端测得准，为什么还要社区源？**
因为"谁进候选集"和"先试谁"是两件事：

| | 谁决定 | 依据 |
|---|---|---|
| **成员资格** — 哪些 IP 值得试 | 候选来源（DNS + 社区源） | 种子 |
| **优先级** — 先试哪个 | 客户端（`internal/nodepool`） | 它自己测出来的延迟与成功率 |

客户端打分（`nodepool.go`）：

```
v = (1 - 成功率) × 1000 + 延迟(ms) + 连续失败次数 × 200    然后 ±15% 抖动
```

"测哪个快"必须客户端做，但"候选从哪来"谁做都行 —— 社区源只是扩大候选集，
单个源死掉不影响其他，全部死掉退回 DNS 解析。这份冗余是刻意的。

## 中继列表：同样是"硬编来源，动态内容"

Worker 不能连 Cloudflare 自己的 IP，目标是 Cloudflare 承载的站点时必须借第三方
中继（见 [relay.md](relay.md)）。中继列表从社区源动态获取（`forward.js` 的
`RELAY_SOURCE`，1 小时 isolate 内缓存），失败并上内置的 9 个公共中继兜底。

与入口候选同一个哲学：**硬编来源，动态内容，探测兜底**。列表内容说变就变、
说死就死，探测 + 亲和才是自愈机制。

## 数据的跳数

客户端只做选路，不改写应用层数据。两条路径的实际跳数：

### 国内直连（2 跳）

```
浏览器 ──▶ netmaster ──▶ 目标站
```

`netmaster` 调 `net.DialTimeout` 直拨，不经隧道。

### 境外代理 · 非 Cloudflare 目标（3 跳）

```
浏览器 ──▶ netmaster ──TCP+TLS(ECH)+WS──▶ Cloudflare 边缘 (= worker) ──出站 TCP──▶ 目标站
```

worker 与它所在的边缘是同一台机器，中间不占网络跳。

### 境外代理 · Cloudflare 承载目标（4 跳）

```
浏览器 ──▶ netmaster ──▶ Cloudflare 边缘 (= worker) ──▶ 中继 ──▶ 目标站
```

worker 不能连 Cloudflare 自己的 IP（平台限制），必须借第三方中继出站。多这一跳，
而且**这一跳的出口 IP 决定了 Cloudflare 系站点给 200 还是 403**——见
[relay.md](relay.md)。

## 客户端模块

```
cmd/netmaster       子命令入口：serve / nodes / restore（watchdog 为 serve 派生的内部命令）
internal/cache      进程间共享的磁盘缓存（原子写，带年龄）
internal/config     config.json 的查找与解析（flag > 文件 > 默认值）
internal/entry      入口候选：DNS + 社区源（多源并行、逐源容错、缓存兜底）
internal/geoip      CN 网段表：拉取、缓存、按域名判断是否落在国内
internal/route      分流规则：顺序匹配、首条命中
internal/nodepool   入口池：延迟探测、评分、熔断、域名亲和、学到的状态落盘
internal/nodepool/exit.go   出口选择（直连 or 节点），见 routing.md
internal/nodepool/probe.go  节点探测与结果缓存
internal/outbound   内部协议客户端：auth 握手、mux 会话、ECH 出站
internal/proxy      HTTP / SOCKS5 服务端，CONNECT 隧道与失败重放
internal/tlsutil    TLS / ECH 原语与 ECH 短路
internal/sysproxy   Windows 系统代理接管与还原
internal/procwait   等待进程退出（看门狗用）
```

## 服务端模块

```
src/index.js           fetch 入口：只把 WebSocket 升级交给 Forwarder
src/protocol.js        内部协议：会话开帧、mux 帧编解码
src/forward.js         会话转发、mux、出站目标选择、中继亲和、动态中继列表
src/vless.js           —（已删除，VLESS 不是我们的协议）
src/crypto.js          MD5、auth 派生、常量时间比较
src/relayprobe.js      问中继"这个域名经你会返回什么"
src/util.js            withTimeout 等小工具
```

`build.mjs` 把 `src/` 顺序拼接成单个 `_worker.js`（模块顶层名字唯一，剥掉
import/export）。**改代码要改 `src/`，`_worker.js` 是产物。**

## 缓存与持久化

| 位置 | 内容 | 失效策略 |
|---|---|---|
| 客户端 `<缓存目录>/entry-*.json` | 社区优选列表 | 每次启动重拉，失败时任何年龄可用 |
| 客户端 `probe-*.json` | 各节点延迟 | 10 min |
| 客户端 `state-*.json` | 域名亲和 + 直连冷却 | 节点失效即弃；地址对不上候选集即弃 |
| 客户端 `geoip-*.json` | CN 网段表 | 7 天 |
| 服务端 D1 `relay_binding` | 域名 → 上次可用的中继 | 长期（按 `updated_at` 判断） |

D1 只有一张表。入口列表、UUID 机制都已被候选来源与 PASSWORD 派生取代，
没有留下历史包袱。

客户端启动只读缓存不发网络请求（社区源拉取有界等待是唯一的例外），缺什么
后台补什么。

## 测试

| 位置 | 覆盖 |
|---|---|
| `server/test/crypto.mjs` | MD5 已知向量、auth 派生、常量时间比较 |
| `server/test/protocol.mjs` | 会话开帧解析/构造、mux 帧 |
| `server/test/integration.mjs` | auth 握手、多会话复用一条 WS、交错数据 |
| `server/test/control.mjs` | 出站失败时必须回控制帧告知客户端原因 |
| `server/test/relay.mjs` | 中继源解析、候选顺序、亲和、`relays: []` 禁用 |
| `server/test/devserver.mjs` | 真实 forward.js 的本地 WS 服务，供 Go 端到端测试 |
| `client/internal/outbound/mux_integration_test.go` | Go 客户端 ↔ devserver 端到端 |
| `client/internal/proxy/replay_test.go` | CONNECT 重放：被阻断要回退、直连正常不许回退 |
