# NetMaster 产品需求文档（PRD）

版本：v1.1（冻结基线）
日期：2026-10-02

变更记录：
v1.1 —— 砍掉 NONCE 机制与 Nonce DO、代码混淆、/stats 端点；补每流开帧格式（v1.0 缺口）；
ECH 写入传输规格；CMD 字段删除（仅 TCP CONNECT 一种命令）；TS 窗口放宽至 ±300s；
IP 优选砍掉速度测试；macOS/Linux 系统代理列为可选增强；新增 M0 平台核验阶段。
v1.0 —— 冻结基线。

---

## 1. 项目概述

NetMaster 是基于 Cloudflare Worker + Durable Objects 的轻量级代理工具。客户端使用 Go 实现，服务端运行在 Cloudflare Workers 免费版。采用自定义极简协议，仅支持 TCP，具备多路复用、客户端分流、服务端智能出口选择、启动时 IP 优选、定时健康检查。零服务器成本，端到端可控。

---

## 2. 目标

· 服务端完全运行在 Cloudflare Workers 免费版，Session DO 承载 WebSocket 会话，不依赖自建 VPS。
· 协议自定义、极简，不兼容现有代理协议。
· 客户端支持 SOCKS5（仅 CONNECT）/HTTP 入站，端口自动顺延与系统代理配置。
· 出站连接使用 ECH 隐藏 SNI（与普通 TLS 竞速，失败自动回退）。
· 多路复用：单条 WebSocket 承载多条逻辑流，带每流缓冲上限。
· 服务端按目标地址智能选择出口，无需客户端指定路由。
· Cron 每小时健康检查，维护 NAT64 前缀与 ProxyIP 连接池。
· 客户端启动时并行拉取内置订阅源并执行 IP 优选。

非目标：不支持 UDP；不做链式代理；不做多用户、计费、审计；不保证向后兼容；不做代码混淆与反分析；不提供 HTTP 诊断端点。

---

## 3. 系统架构

### 3.1 客户端（Go）

· 入站层：SOCKS5（仅 CONNECT）、HTTP（可选，CONNECT）。
· 分流层：规则匹配，输出直连或代理。
· 多路复用层：单条 WebSocket 承载多条逻辑流，按 STREAM_ID 分发。
· 传输层：wss://，出站优先 ECH（隐藏 SNI，复用 client/internal/tlsutil 既有实现），ECH 不可用或超预算时回退普通 TLS。
· 本地服务：端口顺延、系统代理配置、IP 优选、订阅拉取、自动重连。
· 订阅层：内置两类源（规则集、优选 IP），并行拉取、分别解析。

### 3.2 服务端（Worker + Durable Objects）

· Worker 入口：路径校验（固定 /）、WebSocket 升级、转交 Session DO；非 WebSocket 请求返回 404（空 body、无额外头，避免指纹）。无任何 HTTP API 端点，诊断走 DEBUG 日志 + wrangler tail。
· Session DO：每连接一个；Hibernation 承载 WebSocket；首帧解析、认证；多路复用分发；6 并发槽位竞速；出口层。
· Router DO：全局单实例（保留分片接口），目标→出口路径映射持久化。
· Cron：每小时健康检查，写 KV。

### 3.3 存储层

· Session DO 内存：会话级流表、计数。
· Router DO（SQLite）：目标→出口路径映射。
· KV：仅存 Cron 健康结果（NAT64 前 4 + ProxyIP 前 4），last-good-wins。
· 客户端本地：规则集缓存、节点池、日志。

---

## 4. 协议定义

### 4.1 连接建立

客户端与服务端建立一条 WebSocket 连接（wss://），承载多条逻辑流。路径固定为 /。鉴权是连接级的：仅首帧携带 AUTH，后续开帧与数据帧零鉴权开销。

### 4.2 首帧（Client → Server，连接级，同时打开流 1）

```
AUTH(16) | TS(8) | STREAM_ID(4) | ATYP(1) | ADDR | PORT(2)
```

字段 说明
AUTH HMAC-SHA256(PASSWORD, TS ‖ STREAM_ID ‖ ATYP ‖ ADDR ‖ PORT)[:16]
TS Unix 秒，服务端校验 ±300 秒（无 NONCE 存储，窗口宽度只影响理论重放面；放宽以容忍客户端时钟偏差）
STREAM_ID 4 字节大端序，范围 [1, 0xFFFFFFFE]；首帧必须 ≥ 1，0 号拒绝
ATYP 0x01 IPv4，0x02 域名，0x03 IPv6
ADDR 对应地址
PORT 目标端口，大端序

无 MAGIC、无 VER、无 CMD（本版仅 TCP CONNECT 一种语义）、无 ROUTE。

### 4.3 开帧（Client → Server，流 2 及以后）

```
STREAM_ID(4) | ATYP(1) | ADDR | PORT(2)
```

复用已认证的 WebSocket 连接打开新流，零额外鉴权开销。与数据帧的区别在于携带目标地址。

### 4.4 响应帧（Server → Client，每流一个）

```
STREAM_ID(4) | STATUS(1)
```

STATUS 含义 触发条件
0x00 成功 —
0x01 首帧/开帧格式或认证错误 HMAC 错、TS 超窗、STREAM_ID 非法（0、0xFFFFFFFF、与在用流重复）、ADDR 超长、ATYP 非法
0x02 目标禁连 私网 IP、CF 网段、端口 25
0x03 出口全部失败 竞速全部失败

客户端在收到该流的 STATUS 0x00 之前不得发送数据帧。失败时服务端紧接控制帧 CLOSE 并静默关闭该流。

### 4.5 数据帧与控制帧

数据帧：STREAM_ID(4) | PAYLOAD，STREAM_ID ∈ [1, 0xFFFFFFFE]。

控制帧：0x00000000 | CTRL_TYPE(1) | ...

CTRL_TYPE 含义
0x00 应用层心跳（保留定义，本版不使用）
0x01 流关闭：0x00000000 | 0x01 | STREAM_ID(4)
0x02 保留

约束：

· 单帧 PAYLOAD 上限 64 KB。客户端每次从出站 socket 读 ≤ 64 KB 再封帧；服务端收到 PAYLOAD > 64 KB 的数据帧视为协议违规，关闭该流。（平台 WS 收包上限现为 32 MiB，本协议 64 KB 为主动设计约束，不随平台变化。）
· 控制帧总长 ≤ 9 字节。
· 0x00000000 控制帧专用；0xFFFFFFFF 永久保留。
· STREAM_ID 回绕：递增到 0xFFFFFFFE 后回绕到 1，跳过 0 与 0xFFFFFFFF。

### 4.6 流关闭与保活

· 保活：客户端每 30 秒发送 WebSocket 协议层 Ping 帧；边缘自动 Pong，不唤醒 Session DO。
· 客户端判死：Ping 发送失败，或连续 2 个周期未收到 Pong，判定连接死亡，触发重连。
· 服务端判死：仅在 DO 醒着时执行；180 秒未收到任何数据帧则关闭该 Session。休眠期间不执行，会话死亡由客户端下次发起流时暴露。
· 流关闭：任一方可发送控制帧 CLOSE；收到对端 CLOSE 后停止发送，待本地发送缓冲清空后关闭该流出站 socket。
· WebSocket 整体关闭时所有流一并关闭。

### 4.7 背压

· 每流接收缓冲上限 1 MiB，超过即发送 CLOSE 并丢弃该流出站连接，不影响其他流。
· 单条 WebSocket 存在队头阻塞可能，缓冲上限保证阻塞只影响单条流。

---

## 5. 鉴权

· 客户端与服务端共享 PASSWORD，由用户手动配置，不做任何长度或复杂度限制，用户自负。
· AUTH 使用 HMAC-SHA256，不传原始 PASSWORD；TS 参与签名防篡改。
· AUTH 比较使用常量时间函数（Workers 原生提供 crypto.subtle.timingSafeEqual）。
· 重放防护有意从轻：连接在 TLS 之内，能拿到明文帧的重放者本就能终止 TLS，重放不构成现实威胁。TS ±300 秒仅作为廉价的一致性校验，不引入 NONCE 存储与去重 DO。
· 认证失败、TS 超窗、STREAM_ID 非法均返回 STATUS 0x01 并静默关闭。
· 协议层安全仅保证 HMAC 签名与 TS 校验两项。弱密码风险由用户承担。
· 无版本协商：未来协议升级为破坏性变更，客户端与服务端必须同时更新；错误版本的帧呈现为 STATUS 0x01（与密码错误不可区分）。

---

## 6. 客户端（Go）

### 6.1 功能

功能 描述
SOCKS5 入站 仅 CONNECT；收到 UDP ASSOCIATE 直接返回 command not supported
HTTP 入站 可选，CONNECT
端口自动顺延 默认 1080，被占用依次尝试 1081…（上限 1090；仍失败则报错退出）
系统代理配置 无已有实例则配置；有实例则跳过
ECH 出站 优先 ECH 隐藏 SNI，失败或超预算（2s）回退普通 TLS；连续失败进入 60s 短路
订阅拉取 启动时并行拉取两类内置源：规则集、优选 IP
IP 优选 启动时执行
分流 仅直连或代理，不拒绝
域名传递 不拦截 DNS，域名原样传服务端
多路复用 单条 WebSocket 承载多条流
自动重连 指数退避
观测 见第 12 节

### 6.2 系统代理与 UDP 说明

系统代理仅接管 TCP 流量。Windows WinINET、macOS 网络扩展、Linux gsettings 代理均不转发 UDP。即使 SOCKS5 支持 UDP ASSOCIATE，系统代理实现通常也不会调用。

NetMaster 已删除 UDP 能力，客户端 SOCKS5 对 UDP ASSOCIATE 直接返回不支持。这是设计预期内的行为，客户端需在文档中明确提示：

· QUIC 流量不会被代理，建议在浏览器中禁用 QUIC（chrome://flags/#enable-quic 设为 Disabled），强制回落 TCP。
· DNS 查询若走系统解析器可能泄漏；客户端不拦截 DNS，由用户自行决定是否配置加密 DNS。
· 游戏、视频通话等 UDP 应用不在覆盖范围内。

### 6.3 分流规则

匹配顺序：私网/回环 CIDR → 域名精确 → 域名后缀 → 关键字 → GeoSite → IP CIDR → GeoIP → 端口 → 兜底。

· 私网/回环强制 Direct，最高优先级（服务端 connect() 禁连私网）。
· 输出：Direct 或 Proxy。

### 6.4 用户手动配置

PASSWORD、Worker 地址。其余配置全部内置，用户无需填写订阅 URL。高级用户可通过配置文件覆盖内置源。

### 6.5 启动流程

## 1. 检查是否已有实例运行。
## 2. 并行拉取两类内置源：规则集、优选 IP。
## 3. 解析并加载规则集；失败用上次缓存，无缓存用内置硬编码兜底。
## 4. 执行 IP 优选。
## 5. 绑定本地端口，被占用则顺延。
## 6. 无已有实例则配置系统代理；有则跳过。
## 7. 建立 WebSocket 连接（懒建立：首条代理流到达时）。
## 8. 开始监听。

### 6.6 IP 优选

## 1. 从优选 IP 源拉取列表。
## 2. 并发 TCP 延迟测试，取最快的 16 个为节点池，按延迟排序。
## 3. 随机选 1 个为当前节点；失败剔除并重新随机。
## 4. 池耗尽打印警告，不再重新优选。

（吞吐速度测试列为 TODO：v1 仅延迟测试，启动预算内做不完 16 个候选的吞吐测量。）

### 6.7 断线重连

· 触发：WebSocket 断开、Ping 失败、节点连接失败。
· 策略：指数退避 1s → 2s → 4s → 8s → 16s → 32s，每次 ±20% 抖动。
· 连续失败 5 次判定节点死亡，剔除并从节点池剩余随机重选，退避清零。
· 断开期间新流：进入等待队列（上限 128 条、超时 10s），重连后批量发起；超时向 SOCKS5 客户端返回失败。
· 不做会话恢复；在途流直接关闭，上层应用自行重试。
· 节点池耗尽后不再重连，打印警告并保留直连分流能力。

---

## 7. 服务端智能分流与出口

### 7.1 组件职责

组件 职责
Worker 入口 路径校验（固定 /）+ WebSocket 升级；非 WS 请求 404 空 body；生成随机 UUID 作 Session DO 的 idFromName 参数
Session DO 每连接一个；Hibernation 承载 WS；首帧解析 + 认证；多路复用分发；6 并发槽位竞速；出口层；会话计数
Router DO 全局单实例（保留分片接口）；目标→出口路径映射持久化；异步批量 flush
KV 仅存 Cron 健康结果（NAT64 前 4 + ProxyIP 前 4），last-good-wins
Cron 每小时健康检查，更新 KV

Session DO 分配：Worker 生成随机 UUID 作 idFromName 参数，每连接独立 Session DO，客户端不感知。

### 7.2 智能分流流程

每条逻辑流：

## 1. 查询当前 Session DO 内的会话级内存缓存（仅本 WebSocket 会话内有效；跨会话复用完全依赖 Router DO）。
## 2. 未命中，查询 Router DO。
## 3. Router DO 命中，写回内存，使用该路径。
## 4. 均未命中，启动 6 并发槽位竞速。

缓存失效：

· 出口连接建立失败，或连接建立后 3 秒内无首字节数据返回时，删除 Router DO 中该目标的映射，回退到竞速。
· 条目 TTL 1 小时（与 Cron 周期对齐）；updated_at + 3600 < now 视为未命中，避免抖动目标长期滞留死映射。

### 7.3 竞速参数

参数 值 可调
单槽超时 1.5s 环境变量
全局超时 3s 环境变量
交错启动间隔 120ms 环境变量

交错顺序：

```
NAT64[0], ProxyIP[0], NAT64[1], ProxyIP[1], …
```

任一成功，回收全部槽位，成功路径写回内存与 Router DO（异步）。失败或超时槽位立即回收，后续路径进入。所有槽位失败，返回 STATUS 0x03。

### 7.4 出口定义

· 直连：Session DO 内 connect() 目标。
· NAT64：仅支持 /96 前缀（RFC 6052）。域名目标需先经 DoH 解析出 IPv4，再合成 IPv6。解析结果随路径写入 Router DO 缓存。不让客户端预解析，保持“域名原样传递”。
  · DoH 通过 fetch() 发起，走 Worker 的 HTTP 出站通道，不受 connect() 禁连清单（含 CF IP 段）约束；若 fetch 1.1.1.1 在特定 PoP 受限，回退 dns.google 或 security.cloudflare-dns.com 等备选 DoH 端点。
· ProxyIP：HTTP CONNECT，本版仅实现这一种。KV 节点条目预留 type 字段（默认 "http-connect"）。

不使用链式代理。

### 7.5 Router DO 设计

· 全局单实例起步，保留分片接口 idFromName("router:" + shardId)。
· 分片触发阈值：映射行数 > 50 万或请求速率持续 > 100 req/s，启用 16 分片（按目标主域哈希）。
· 存储：SQLite 表 routes(target_hash TEXT PRIMARY KEY, egress_type TEXT, egress_id TEXT, updated_at INTEGER)。
· target_hash = 目标域名（小写）SHA-256 前 16 字节十六进制。
· 异步批量 flush：写入先返回，攒 5 秒或 50 条 flush 一次。定时器用 DO Alarm：写入时若无待定 Alarm 则 setAlarm(now+5s)；满 50 条立即 flush。
· 立即 flush 后若队列为空，取消待定 Alarm，避免空转唤醒消耗行写配额。
· flush 失败重试 1 次，仍失败丢弃该批（缓存可从竞速重建）。进程驱逐时未 flush 数据接受丢失。

### 7.6 EWMA 参数

· α = 0.3
· 延迟 : 成功率 = 7 : 3

---

## 8. 配置与订阅

### 8.1 配置项

配置项 位置 更新方式
PASSWORD 客户端配置 / Worker Secrets 手动 / 部署时
Worker 地址 客户端配置 手动
内置源覆盖 客户端配置（可选） 手动
NAT64 前缀列表 服务端 Cron 侧内置源 每小时
ProxyIP 列表 服务端 Cron 侧内置源 每小时

WebSocket 路径固定为 /，不可配置。

### 8.2 内置订阅源

类型 消费方 格式
规则集 客户端分流引擎 Clash RULE-SET / sing-box srs / JSON
优选 IP 客户端 IP 优选 纯文本 ip:port
NAT64 前缀 服务端 Cron 纯文本 CIDR
ProxyIP 服务端 Cron 纯文本 ip:port 或域名:port

规则集源：默认 Loyalsoldier/clash-rules（每日构建，direct/proxy/reject/private/apple/gfw 分类）；备选 Dreista/sing-box-rule-set-cn、KaringX/karing-ruleset。

优选 IP 源：默认 ymyuuu/IPDB（ipdb.api.030101.xyz/?type=bestcf，小时级更新）；备选 LancelotRar/best-cf-ips（best-cf-ip-collected.txt，每 3 小时更新）。

NAT64 前缀源：默认 nat64.net 全部前缀（含 2a00:1098:2b::/96 等 5 个位置）；备选 Trex（2001:67c:2b0::/96）、level66（2001:67c:2960::/96）、IPng（2a02:898::/96）。仅支持 /96。

ProxyIP 源：默认之一 CMLiussss（域名型，ProxyIP.US.CMLiussss.net 等）；默认之二 ymyuuu/IPDB bestproxy（IP 型）。两者互为备份，格式同为纯文本，解析器零新增。

解析器：Clash RULE-SET（逐行）、sing-box srs（引入 sing-box/common/rule-set）、纯文本 ip:port、纯文本 CIDR。默认仅启用 Clash 格式；sing-box 格式可选。

覆盖与兜底：用户可覆盖任一内置源，留空用默认；任一源失败不影响其他源；规则集失败用上次缓存，无缓存用内置硬编码；节点类源失败用内置硬编码兜底。

### 8.3 数据格式示例

```
# Clash RULE-SET
DOMAIN-SUFFIX,google.com
DOMAIN,example.com
IP-CIDR,1.1.1.0/24
```

```json
{
  "version": 2,
  "updated_at": 1791360161,
  "rules": [
    {"type": "cidr", "value": "10.0.0.0/8", "action": "direct"},
    {"type": "domain_suffix", "value": "google.com", "action": "proxy"},
    {"type": "ip_cidr", "value": "1.1.1.0/24", "action": "direct"},
    {"type": "port", "value": "25", "action": "direct"}
  ],
  "fallback": "proxy"
}
```

```
# 优选 IP / ProxyIP（纯文本，支持 # 注释）
104.16.1.1:443 US
104.17.2.2:2053 HK
ProxyIP.US.CMLiussss.net:443
```

```
# NAT64 前缀
2a00:1098:2b::/96
2001:67c:2b0::/96
```

---

## 9. 健康检查（Cron）

> **已由附录 A1 / A6 修订**：NAT64 出口移除；Worker Cron 整体移除，本节所述流程由
> GitHub Actions 的 refresh-relays.yml 承担（成功率 desc → 延迟 asc，非 EWMA；
> KV 只剩 proxyip:top 一个键）。以下为冻结基线原文。

每小时一次：

## 1. 从内置源拉取最新 NAT64 前缀与 ProxyIP 池。
## 2. 剔除回指本 Worker 地址的 ProxyIP 条目（TCP Loop 检测会拒绝回连自身）。
## 3. 全部节点入队，6 并发槽位依次消费；每个任务完成或超时立即回收槽位。
## 4. 记录延迟与成功率，按 EWMA 排序。
## 5. NAT64 取前 4、ProxyIP 取前 4，共 8 个写入 KV，不记录其他节点。
## 6. 写入策略：last-good-wins，本轮成功才覆盖，失败保留上一轮。

探测方法：

· NAT64：合成 ipv4only.arpa 或 1.1.1.1 的 IPv6 地址测往返。
· ProxyIP：完成 HTTP CONNECT 握手后请求轻量 HTTP 端点。

约束：Cron 触发每次运行最多 50 次外部子请求（Workers 免费版），节点超预算时分多次执行。

---

## 10. 限制与应对

M0 平台核验已完成（2026-10-02，E1-E5；同日第二轮补 E6-E9，结论详见仓库
docs/m0-findings.md）。所有实测结论已回写本表；架构按附录修订定稿并实施完毕（v0.2.0）。

限制 现状 应对
并发出站连接 【M0 实测 E2】DO 内 12 条并发 connect() 全部成功，"每请求 6 条"限制不适用于 DO 内 TCP socket 6 槽位竞速保留为预算控制而非性能上限
connect() 禁连 CF IP 段、localhost、私网、端口 25；无 UDP 出站；禁回连自身（TCP Loop）；【M0 实测 E3】IPv6 出站不支持（IPv6 字面量与 NAT64 合成地址一律立即失败）→ NAT64 出口无法实现，需从架构中移除；【M0 实测 E6】80 端口一律禁拨（与目标是否域名、是否 CF 网段无关）；【M0 实测 E7】E6/E7 拒绝的错误文案相同，区分只能靠自行解析比对网段 ProxyIP 兜底成为 CF 承载目标的唯一出口；客户端私网强制 Direct；refresh-relays 剔除回指条目；直连路径先 DoH 解析再逐 IP 拨号（判定点在 IP 层）；验收目标一律 443 + 非 CF 托管
免费版每日 100k 请求 【M0 实测 E1】WS 消息按 ≈1:1 计入请求，未观察到 20:1 折算（出站帧可能同样计费）mux 只省建连不省消息量；协议层吝啬帧数：数据帧尽量满 64KB、心跳只走 WS 协议层 Ping（不产生 DO 消息）
脚本体积 1 MB（压缩） v0.2.0 产物 _worker.js ≈ 59 KB，余量充足
DO WS 接收消息 32 MiB 单帧 ≤ 64 KB（主动设计约束）
SQLite 行写 免费版 100k 行/日；主要来源：Router DO flush 个人规模无压力
SQLite 行读 免费版 5M 行/日；每未命中流 1 读（会话级缓存摊薄） 余量充足
SQLite 存储 单 DO 10 GB；账户总存储免费版 5 GB NetMaster 用量（< 1 MB）远低于限档
子请求 【M0 实测 E8】每 invocation 50 个（免费版），一条活跃 WS 会话是一次长驻 invocation —— 实际是"每条连接一生"的总额度（实测第 26 对 fetch+connect 耗尽）DNS 进程内缓存（TTL 5min）；直连失败记忆（本连接内）；成功建连 30 次后 close(1000,"budget") 优雅断开，客户端重连换新预算（客户端重连退避 + 等待队列是该机制成立的前提）
CPU Worker 10ms/请求；DO 30s/消息（未单独复核） 隧道逻辑落在 DO 内，I/O 密集
DO 休眠与出站 socket 【M0 实测 E4】有出站 socket 的 DO 不休眠（挂起 I/O 阻止休眠），socket 关闭后才可休眠；空闲 socket 由对端在数十秒内关闭 "休眠期间流保活"不成立也无需成立：零流空闲 WS 走 Hibernation 不驻留内存；每流死亡走 CLOSE 正常处理
可观测性 【M0 实测 E9】DO 内的 console 输出在 wrangler tail 上完全不可见（worker 入口可见；DO 间 fetch 可见）诊断走 KV 通道（debug:lastExit）；"是否发生"查 KV 时间戳不查 tail；新增诊断默认写 KV 不写 console
KV 读 ~10ms、最终一致 竞速候选（proxyip:top）与调试通道使用，不在每帧热路径
系统代理不转发 UDP 平台限制 客户端文档提示禁用 QUIC

来源注释：Durable Objects（SQLite 后端，单 DO 10 GB）已在 Workers 免费计划可用且免费计划存储不收费，依据 Cloudflare Changelog 2025-04-07 与 2025-12-12。平台限额可能调整，部署前以 developers.cloudflare.com 当前页面为准。

---

## 11. 安全

· AUTH 使用 HMAC-SHA256，不传原始 PASSWORD；比较使用常量时间函数。
· TS ±300 秒校验；重放防护有意从轻（理由见第 5 节），不引入 NONCE 存储。
· 认证失败、TS 超窗、STREAM_ID 非法均返回 STATUS 0x01 静默关闭。
· PASSWORD 无长度或复杂度限制，用户自负。
· Secrets 不落日志；客户端日志默认脱敏目标地址。
· 无版本协商：未来协议升级为破坏性变更，错误版本的帧呈现为 STATUS 0x01（与密码错误不可区分）。
· 使用 Cloudflare 小号部署，绑定自定义域名。

---

## 12. 可观测性

无任何 HTTP 诊断端点（曾评估 /stats，砍掉：DEBUG 日志 + wrangler tail 已覆盖调试需求，且维持零端点的攻击面）。

客户端：

· 日志分级（默认 INFO，--verbose 开 DEBUG）。
· 关键事件日志：tunnel established via node x、出口切换、重连、订阅源拉取状态。
· 服务端开关 DEBUG=1 后经 wrangler tail 输出结构化 JSON 日志。

服务端：

· Session DO 会话级计数（流数、字节、各出口占比），会话结束写结构化日志，不额外持久化。
· 出口失效与竞速结果落日志（bad relay 遗忘、STATUS 0x03 原因）。

---

## 13. 合规风险

在 Workers 上运行通用代理游走在 Cloudflare 服务条款边缘，账号可能被封。应对：个人使用、低流量；小号部署 + 账号隔离；自定义域名；封禁不影响本地直连分流；文档明示仅供个人技术学习用途。

---

## 14. 里程碑与验收标准

阶段 内容 量化验收
M0 平台核验 WS 消息计费折算（20:1）；DO 并发出站连接上限语义；connect() IPv6 出站；免费版 DO/SQLite/Hibernation/CPU 限额 每项产出实测结论写回 §10；任一不成立则先修订架构再冻结
M1 协议 + Go 客户端 SOCKS5 + Worker→Session DO + 直连出口 curl 经代理访问 HTTP/HTTPS 成功；首帧/开帧解析单测覆盖全部字段与失败码；认证失败 / TS 超窗均返回 0x01 静默关闭
M2 多路复用 + Hibernation + 协议层 Ping + 自动重连 单 WS 并发 ≥ 100 条流；WS 强断后 3s 内自动重连；空闲 5 分钟会话存活且 DO 无唤醒
M3 NAT64 + ProxyIP 出口 + 6 槽竞速 + Router DO 缓存与失效 对 CF 托管目标走 NAT64/ProxyIP 成功率 ≥ 99%；映射命中后建流 P50 ≤ 直连 + 30ms；异步 flush 闭环；缓存条目失效后自动回退竞速
M4 客户端分流 + 端口顺延 + 系统代理 + IP 优选 + 两类源解析 500 条规则匹配 P99 < 1ms；优选全流程 ≤ 10s；端口被占自动顺延；两类源并行拉取成功（Windows 系统代理必做；macOS/Linux 顺手实现，不做则 --manual）
M5 Cron 健康检查 + KV last-good-wins 节点 ≤ 50 时单次 Cron 完成；评分写入 KV 且下一周期可读到；失败轮保留上轮数据；回指 ProxyIP 被剔除
M6 文档与部署脚本 一键部署脚本在全新账号跑通；README 快速开始与实际一致

---

## 15. 术语

· NAT64：IPv4/IPv6 转换网关，仅支持 /96 前缀。
· ProxyIP：第三方 HTTP CONNECT 反向代理 IP。
· Durable Objects（DO）：Cloudflare 单实例强一致的有状态边缘对象。
· Hibernation：DO 的 WebSocket 休眠 API，空闲连接不驻留内存、不计时长。
· Session DO：承载单条 WS 会话的 DO，每连接一个。
· Router DO：全局目标→出口路径映射 DO。
· EWMA：指数加权移动平均，用于健康评分。
· 并发槽位：服务端同时尝试的出口连接数上限（6）。
· STREAM_ID：多路复用中标识逻辑流的编号。
· IP 优选：从社区列表筛选 Cloudflare IP。
· last-good-wins：Cron 写入策略，失败轮保留上轮数据。
· 内置源：客户端与服务端内置的社区订阅源，共四类——客户端消费规则集与优选 IP 两类，NAT64 前缀与 ProxyIP 两类由服务端 Cron 消费。

---

本文档为冻结基线，M0 完成并回写结论后可直接作为 M1 开发依据。实现细节变更记入代码仓库文档，PRD 仅在协议或架构变更时递增版本。

---

# 附录：v2 实施修订记录

日期：2026-10-02。本文正文（v1.1）保持冻结，不做改动；以下条目是 M0 平台核验之后定稿的
架构修订，**覆盖正文对应章节**。实现细节以
`docs/architecture.md` / `docs/relay.md` / `docs/limitations.md` 与 v2 分支代码为准。

## A1. 砍掉 NAT64 出口（覆盖 §2、§3.3、§7.1、§7.3、§7.4、§8.1、§8.2、§9、§14-M3、§15）

- 依据：M0 E3 实测。Workers `connect()` **不支持 IPv6 出站**——IPv6 字面量与 NAT64 合成
  地址一律 <2ms 立即失败，连拨号都没发生；level66 `2001:67c:2960:6464::/96`、well-known
  `64:ff9b::/96`、Trex `2001:67c:2b::/96`、nat64.net `2a00:1098:2b::/96` 四个前缀全部如此，
  IPv4 对照组 4ms 成功。不是前缀选择问题，是运行时能力缺失。
- 修订：NAT64 出口从架构中移除。Cloudflare 承载目标的出口**只剩 ProxyIP 中继一类**
  （`connect()` → 中继 → HTTP CONNECT）。
- 连带修订：竞速槽位不再是"NAT64 / ProxyIP 交错"，而是**全部为 ProxyIP 候选**，参数
  （6 槽 / 1.5s / 3s / 120ms）不变；Cron 不再维护 NAT64 前缀池，只维护 ProxyIP 池
  （`proxyip:top`）；"剔除回指条目"逻辑保留。

## A2. WS 消息按 ≈1:1 计费（覆盖 §10）

- 依据：M0 E1 实测。客户端向 Hibernation DO 发精确数量的消息，与 GraphQL analytics 对照：
  13:00–14:00 UTC 整点桶 **829 requests**。若 20:1 折算成立，该数字应在 ~75。未观察到
  任何折算。
- 修订：**WS 消息按 ≈1:1（或更差）计入免费版每日 10 万请求**。多路复用省下的只是 WS 建连
  成本，每帧仍是一条请求；"mux 摊薄请求量"的叙事不成立。
- 协议层约束（已实现，写进 §10）：数据帧尽量满帧（单帧上限 64 KB）；心跳只走 WebSocket
  **协议层 Ping**（边缘自动应答，不产生 DO 消息、不唤醒 DO，不计请求）；废除逐帧 ACK 与
  应用层心跳控制帧（`CTRL_TYPE 0x00` 保留但未使用）；认证是连接级一次，后续流零鉴权开销。

## A3. D1 删除，中继亲和改用 Router DO（覆盖 §3.3、§7.5）

- 修订：**D1 已从架构中删除**。目标 → 出口路径的跨会话复用由 Router DO 承载：SQLite 表
  `routes(target_hash, egress_type, egress_id, updated_at)` + Alarm 异步批量 flush（攒 5 秒
  或 50 条，同一 hash 只留最新一条）。
- 连带修订：部署不再需要 `wrangler d1 create` 与 `schema.sql`；Release 产物不再包含
  `schema.sql`（release.yml 已同步）。
- 不变：条目 TTL 1 小时、flush 失败重试 1 次后丢弃、进程驱逐丢失未 flush 数据可接受、
  分片接口 `idFromName("router:<shard>")` 与 16 片阈值保留。

## A4. 无 HTTP 诊断端点，调试走 DEBUG + wrangler tail（确认 §12）

- 正文已写明"无任何 HTTP 诊断端点"，实施再次确认：Worker 入口只认 `pathname === "/"` 且
  `Upgrade: websocket`，其余一律 404 空 body、无额外头（避免指纹）。
- 调试路径：Worker 变量 `DEBUG=1` + `npx wrangler tail`，日志前缀 `[session]` /
  `[router]` / `[cron]`。部署是否健康由"客户端能不能连上"直接回答；客户端 serve 启动后在
  后台做一次真实建流（目标 `www.google.com:443`，A7 按 E6/E7 从 `example.com:80`
  改来），结果补一行
  `tunnel established via node <addr>`。
- CI 无部署后验证：runner 在美国、用户在大陆，runner 能通不代表用户能通。

## A5. 其余按 M0 结论保留的部分

- 并发出站连接（M0 E2：单 DO 12 条并发 `connect()` 全部成功）——6 槽竞速保留为**预算控制**
  而非性能天花板。
- DO 休眠语义（M0 E4）：**有出站 socket 的 DO 不休眠**，socket 关闭后才可休眠；空闲 socket
  由对端在数十秒内关闭。正文 §4.6"休眠期间流保活"的对象不存在，无需对抗：零流的空闲 WS
  走 Hibernation 不驻留内存，每流死亡走 CLOSE 正常处理。
- 出口层其余参数、Router DO、KV last-good-wins 全部保留。（健康检查的形态与 EWMA
  评分见 A6：已随 Cron 一起移出 Worker。）

## A6. 移除 Worker Cron，中继池刷新改由 GitHub Actions 定时任务承担
（覆盖 §2、§3.3、§7.1、§8.2、§9、§10、§14-M5、§15）

- 日期：2026-10-02。
- 依据：免费版 Cron 触发**不可靠**——实测只有整点触发，且整点也可能漏发。而 KV 里的
  `proxyip:top` 决定 CF 承载目标的出口质量，池子该更新时没更新，用户看到的就是"时好时坏"。
  同时免费版 Cron 触发每次运行只有 50 次外部子请求，逼得探测必须分批游标续跑（正文 §9
  的分批流程即由此而来）。
- 修订：Worker 侧 Cron 整体移除 —— 删除 `server/src/cron.js`、`index.js` 的 `scheduled`
  handler、`wrangler.toml` 的 `[triggers] crons`。替代是 `.github/workflows/
  refresh-relays.yml`：`schedule` 每小时一次 + `workflow_dispatch`，跑
  `server/tools/refresh-relays.mjs`（拉源 → 并发探测 → 排序 → 写 KV `proxyip:top`）。
- 排序口径变化：**不再做 EWMA**（α=0.3、延迟:成功率 = 7:3）。runner 每轮都是全新观测、
  没有历史可平滑，EWMA 在这里没有可平滑的对象；改为"成功率 desc → 平均延迟 asc"。
  last-good-wins 保留（本轮全败不写、保留上一轮、退出码 0）。
- 影响面（M5 里程碑的形态变化）：M5 从"Worker 内每小时健康检查"变成"仓库侧定时任务"。
  - 验收口径随之改变：不再有 `[cron]` 服务端日志可查，排障现场是 Actions 的 run 页面；
    `cron:lastRun` / `cron:cursor` / `cron:pending` 三个 KV 键不再存在（探测在 runner 上
    一次跑完，没有分批续跑的中间状态），KV 只剩 `proxyip:top` 一个键。
  - **`schedule` 是"每小时左右"不是准点**（GitHub 官方说明有几分钟级延迟，高峰期可能更久）；
    仓库连续 60 天无活动时 GH 会**自动停用**定时任务。两者都是预期行为，不是故障。
  - 约束方从 Cloudflare 免费版的 50 次/轮子请求，换成 GitHub 托管 runner 的分钟数
    （私有仓库 2000 分钟/月；每轮约 1 分钟，每小时一轮 ≈ 720 分钟/月）。
- 连带订正：A4 里"日志前缀 `[session]` / `[router]` / `[cron]`"一句中的 `[cron]`
  已失效——Cron 移除后服务端只剩前两个前缀。A4 其余内容（无 HTTP 诊断端点、
  `DEBUG=1` + `wrangler tail`、部署健康由客户端建流回答）不受影响。

## A7. 修订 M3 出口质量判据与 M2 并发口径（覆盖 §14-M2、§14-M3）

- 日期：2026-10-03。
- 依据：正文 §14-M3 的"CF 托管目标成功率 ≥ 99%"写于 NAT64 可用之时。A1 移除 NAT64 后，
  CF 承载目标只剩公共 SNI 中继一类的出口，实测（acceptance run 37063143931 / 37063498440，
  间隔 8 分钟）同一部署 20 轮窗口分别测得 **45% 与 65%**，波动来源是公共中继池本身的
  不稳定：`tls: EOF`（中继 accept 后即断）、**过期证书**（盲转发器拿无域名后备证书应答）、
  403（SNI 路由拒绝）。这不是实现缺陷——竞速、健康排序、KV last-good-wins 均按 §8.2
  实现并有单测——而是免费方案下出口池的质量天花板。
- 修订（§14-M3 验收口径）：
  - **≥99% 降级为"自建中继后的目标"**，不再是公共中继部署的验收线。自建 `http-connect`
    类型中继的路径见 docs/relay.md；自建池稳定的部署可以把验收线调回 99%。
  - 公共中继部署的 CI 验收线改为分级：**< 50% 硬失败**（出口层故障，如 DoH/竞速坏了），
    **50–99% 记 WARNING**（已知池波动），由 `TestLiveCFHostedSuccessRate` 执行。
  - §14-M3 的其余判据不受影响；其中"映射命中后建流 P50 ≤ 直连+30ms"一项的测量手段
    （e2e 第 6 项）随 e2e workflow 修复后获得 CI 覆盖。
- 连带订正（§14-M2 口径）："单 WS 并发 ≥100 条流"在 devserver 对等体上验证（100/100）；
  真实部署上受 E8 预算约束（每条连接约 30 次成功建连即优雅断开重连），CI 对真实 Worker
  验证 20 条流单 WS + 20 条流分摊 4 WS 两种形态。"空闲 5 分钟存活且 DO 无唤醒"由
  e2e 第 8 项（live 模式 300s + 协议层 Ping）承担。
- 随本修订一并订正的事实：客户端首连通验证 `Verify()` 的目标原为 `example.com:80`，
  按 E6/E7 在真实部署上必失败（80 禁拨 + example.com 已迁 CF），已改为
  `www.google.com:443`。

## A8. 首次 live e2e 的发现：M2 空闲判据与 §4.6 矛盾；空闲计时器阻止休眠（覆盖 §14-M2）

- 日期：2026-10-03。背景：e2e.yml 修复后第一次对真实部署跑通（run 37075051878 /
  37075494448），此前该项验收为零覆盖。
- **发现 1（判据矛盾）**：§14-M2 要求"空闲 5 分钟存活且 DO 无唤醒"，但正文 §4.6 规定
  "180 秒未收到任何数据帧则关闭该 Session"。实测（e2e 8.1）：空闲会话在 185s 被
  `close(1000)`，协议层 Ping 不重置计时器（Ping 由边缘应答，不进 DO，见 §4.6 的设计）。
  两个判据不可能同时成立。**修订：以 §4.6 为准**——空闲会话 ~180s 被服务端回收，
  客户端退避重连 + 等待队列兜底（已实现），用户侧表现只是空闲 3 分钟后的下一个请求
  多一次建连。"空闲 5 分钟存活"降级为改进目标（见发现 3）。
- **发现 2（新平台事实）**：e2e 8.1 同时证明空闲会话**从未进入休眠**——session.js 在每条
  消息上重挂的 185s `setTimeout` 是 pending timer，**阻止 DO 休眠**（m0-findings E4 的
  "无出站 socket 才休眠"之外还需"无 pending timer"）。后果：空闲会话全程驻留内存计费、
  到点仍被关闭，两头不占。Hibernation 的收益目前只在"有活动但零流"的窗口内兑现。
  改进方向（未实施，需产品决策）：服务端判死改用 Durable Object Alarm（不阻止休眠、
  到点短暂唤醒检查），或完全依赖 WS close 事件回收死连接；实施后"空闲 5 分钟存活 +
  DO 无唤醒"可以重新作为验收目标。
- **发现 3（并发上限观测）**：单 WS 25 条并发流两轮分别 19/25、18/25 完成请求
  （打开全部 0x00，失败发生在请求阶段，逐流原因已加记录）。e2e 的 mux 并发口径对齐
  A7 修订后的 20 条（`BUDGET_SAFE_STREAMS` 20）；若 20 条下仍复现失败则为 mux/平台
  缺陷，需单独排查。
- **发现 4（对照数据）**：同日 e2e 5.1（ProxyIP 竞速）测得 100%（20/20），而 acceptance
  的 Go 客户端同日测得 45-65%——公共中继池波动幅度极大，进一步支撑 A7 的分级阈值。
- e2e 8.1 的判定随之修订：~180s 被 `close(1000)` 判 PASS（§4.6 行为，并注明未休眠），
  全程存活也判 PASS（休眠语义优先），更早或异常码判 FAIL。
- **补充证据（run 37077244191，23:22 UTC）**：5.1 一轮测得 0/20，20 次失败全部是
  "certificate has expired"——竞速按 TCP 连接速度选赢家，TCP 最快的那个中继整窗都在
  过期证书后端（acceptance 的 Go 侧同样观测到 2026-08-30 到期的证书）。refresh-relays
  的 TLS 探测要等下一小时才能把它剔出 KV；期间该部署的 CF 承载路径实质不可用。
  e2e 5.1 的阈值随之对齐 A7 / live_test.go 的分级口径（≥99% 达标，50-99% WARNING，
  <50% 判出口层故障）。同轮 8.1 另观测到一次 36s 的 1006 异常断开（窗口内仅见一次，
  待复现样本）。
- **补充证据 2（连续两轮 0/20，过期证书持续 >20 分钟）**：run 37078733080 与上一轮
  相隔 19 分钟，20/20 失败原因完全相同（expired cert），而 refresh-relays 的每小时
  TLS 探测照样通过（KV top4 未变）。说明**探测存在盲区**：探测只建一次连接，中继若
  把 ClientHello 路由到带过期证书的后端（按连接或按时间轮换），单次探测探不到。
  改进方向（未实施）：refresh-relays 对每条中继做多次 TLS 探测、或记录探测所用
  servername 的完整证书校验链。
