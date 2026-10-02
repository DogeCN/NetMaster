# M0 平台核验结论

日期：2026-10-02。探针：`m0/`（worker `netmaster-m0`，CI 驱动，运行记录见 GitHub Actions
`m0-probe` workflow，产物 `m0-drive-results`）。复核方式：重跑 workflow_dispatch 即可。

结论先行：**四项实验中两项推翻了 PRD v1.1 的关键假设**——

| # | PRD 假设 | 实测结论 | 判定 |
|---|---|---|---|
| E1 | 入站 WS 消息按 20:1 折算请求量 | **≈1:1 计入请求**，未观察到任何折算 | ❌ 推翻 |
| E2 | DO 内并发出站连接上限语义需确认 | 单 DO 12 条并发 `connect()` 全部成功（worker 内同） | ✅ 成立 |
| E3 | `connect()` 可拨 IPv6（NAT64 的前提） | **IPv6 出站不支持**，一律立即失败 | ❌ 推翻 |
| E4 | DO 休眠期间出站流保活 | **有出站 socket 的 DO 不休眠**；socket 存活与休眠互斥；空闲 socket 由对端关闭 | ⚠️ 假设不成立但结论有利 |

按 PRD §14 的风险门，E1、E3 两项不成立 → 暂停 M1 实施，先修订架构（建议见文末）。

---

## E1：WS 消息计费折算

实验：客户端向 Hibernation DO 发送精确数量的消息，SQLite 持久计数器累计
（upgrade +1、每条消息 +1、close +1），再用 GraphQL analytics
（`durableObjectsInvocationsAdaptiveGroups`）对照账户级请求数。

实测：

- 计数器逐轮 delta 与发送量严格相等：+102（1 upgrade + 100 msg + 1 close）、+50。
- 消静置 40s 后再发，消息照常送达且计数照常累计 → Hibernation 唤醒与计数均正常。
- 13:00-14:00 UTC 整点桶：**829 requests**；其余时段 0（归属干净）。三次完整实验
  发出的 DO 可见事件（含出站回包）总量与 829 同数量级、明显接近 1:1 而非 1/20。
  若 20:1 成立，该数字应在 ~75 而不是 829。

结论：**WS 消息按 1:1（或更差，若出站帧也计费）计入每日 10 万请求配额**。对协议的
影响：多路复用省下的只是 WS 建连成本，每帧仍是一条请求。个人规模（每天数千到数万帧）
在 10 万/日之内，但"mux 摊薄请求量"的叙事不成立，协议帧数要吝啬（合并小包、避免
逐帧心跳）。

## E2：单 DO 内并发出站连接

实验：单 DO 与单 worker 内各并发 12 条 `connect()`（8.8.8.8:443）。

实测：**12/12 成功**（DO 与 worker 均是），延迟个位数毫秒。

结论：文档中"每请求 6 条出站连接"的限制不适用于 DO 内 TCP socket；PRD 的 6 槽位
竞速是设计选择（预算控制）而非性能天花板，mux 稳态不会撞上限。

## E3：IPv6 / NAT64 出站

实验：拨 IPv4 对照组（8.8.8.8:443 成功，4ms）、IPv6 字面量（2001:4860:4860::8888 的
443/853）、CF IPv6（2606:4700:4700::1111）、NAT64 合成地址（2a00:1098:2b::808:808:53）。

实测：**所有 IPv6 目标一律立即失败**（"cannot connect to the specified address"，
<2ms，连拨号都没发生）。IPv4 全通；DoH（fetch 到 cloudflare-dns.com）正常。

结论：**Workers `connect()` 仅支持 IPv4 出站，IPv6 字面量与 NAT64 合成地址均不可用**。
PRD §7.4 的 NAT64 出口（其全部价值就在绕开 connect() 禁连 CF 网段）在该平台上无法
实现，不是调参问题，是运行时能力缺失。

## E4：出站 socket 与休眠/驱逐

实验：DO 内开 TCP socket（带后台读循环），客户端静置 75s，期间 Alarm 在 +20s 触发，
随后检查 socket 状态；E5 变体：客户端断开 WS 后等 45s，新 WS 连回同一 DO 检查。

实测：

- Alarm 在 +20s 触发时 `instanceSockets` 仍非空、isolate 未换 → **开着出站 socket 的
  DO 不会被休眠**（挂起的 socket I/O 阻止休眠）；E5 中客户端断开 45s 后 DO 同样未驱逐。
- socket 的死亡全部来自**对端空闲超时**（Quad9 :53 与 tcpbin :4242 都在数十秒内关闭
  空闲连接）：读端 EOF、写端 `WritableStream has been closed`。
- 没有观察到"运行时在休眠/驱逐时杀 socket"——因为根本到不了休眠那一步。

结论：PRD §4.6"休眠期间流保活"的对象不存在：**socket 存活期间 DO 必然醒着**（计 DO
时长），socket 关闭后休眠才有意义。设计含义：
1. 有在途流时 DO 醒着是必要的，无需对抗；
2. 空闲多路复用连接的出站流会被对端掐掉，协议层要有每流死亡的正常处理（已有 CLOSE）；
3. 零流的空闲 WS 走 Hibernation 不驻留内存——这正是 PRD 预期的形态，成立。

---

## 对 PRD v1.1 的修订建议（待定稿）

1. **砍 NAT64 出口**（E3）。CF 承载目标的出口只剩 ProxyIP 一类：`connect()` 到
   中继 → HTTP CONNECT → 隧道。竞速从 6 槽 NAT64/ProxyIP 交错简化为多 ProxyIP
   候选竞速（参数不变）；Router DO、Cron 健康检查、KV last-good-wins 全部保留，
   只是不再维护前缀池。Cron 的"剔除回指条目"逻辑保留。
2. **明确 WS 消息 1:1 计费**（E1），协议层约束写进 §10：数据帧尽量满 64KB、
   心跳走 WS 协议层 Ping（不产生 DO 消息，免费）、废除逐帧 ACK 类设计。
3. 出口层其余参数（6 槽、1.5s/3s 超时、120ms 交错）按 E2 结论保留。

## 遗留物

- worker `netmaster-m0` 仍部署在账号上（零流量，不耗配额）；确认不需要复核后删除：
  `gh workflow run m0-probe.yml --ref v2 -f teardown=true`，或本地
  `cd m0 && npx wrangler delete --name netmaster-m0 --force`。
- `m0/` 目录与 `.github/workflows/m0-probe.yml` 在 v2 分支保留至架构定稿后删除。
