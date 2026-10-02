# 中继（ProxyIP）

## 为什么需要中继

Workers 的 `connect()` 不能拨 Cloudflare 自有网段（`server/src/exits.js` 的 `CF_V4` /
`CF_V6` 会提前拒绝这类目标，省掉一次注定失败的连接）。而大量站点托管在 Cloudflare 上，
于是这些目标只剩一条出路：借一个第三方 HTTP CONNECT 中继出站。

```
浏览器 ──▶ netmaster ──▶ CF 边缘(Worker / Session DO) ──▶ 中继 ──▶ 目标站
                                                          HTTP CONNECT
```

**这一跳的出口 IP 决定了 Cloudflare 系站点给你 200 还是 403**——站点看到的是中继，不是你。

隧道里跑的是客户端到目标的原始字节。目标是 HTTPS 时，TLS 由客户端端到端完成，中继只
搬运密文：CONNECT 隧道正是为此设计的，我们不碰明文。

### 关于 NAT64

PRD v1.1 给 NAT64 安排的角色正是绕开上面这条限制。但 M0 E3 实测：Workers `connect()`
对 IPv6 字面量与 NAT64 合成地址**一律立即失败**（<2ms，连拨号都没发生），
level66 / well-known / Trex / nat64.net 四个前缀、多个目标全部如此，IPv4 对照组 4ms 成功。
不是前缀选择问题，是运行时不支持 IPv6 出站。**NAT64 出口已从架构中移除**，CF 托管目标的
出口只剩中继一类。详见 [m0-findings.md](m0-findings.md)。

## 中继条目

- 类型：本版只有一种，类型名 `http-connect`（`RELAY_TYPE_HTTP_CONNECT`），写进 Router DO
  的 `egress_type` 字段。
- 格式：`host[:port]`，端口缺省 **443**（`RELAY_PORT`）。中继语义就是反代 CF 的 443，所以
  只收 443 的条目。
- 解析：`parseRelay()` 取最后一个冒号，后面是 1–5 位数字才算端口，否则整串当主机名——
  这样裸 IPv6 字面量不会被误拆。

## 候选来源

竞速按这个顺序组装候选（`race.js` 的 `buildCandidates`），去重后截断到槽位数：

```
① Router DO 命中映射（可选，命中时排第一）
② KV 的 Cron top4（key: proxyip:top，RACE_KV_TOP = 4）
③ 内置兜底列表补齐到槽位数
④ orderByHealth：把 TTL 内刚失败过的挪到尾部，其余保持原顺序
```

第 ④ 步刻意只做一件事——别把刚被拒绝的候选塞进 120ms 内就要启动的前几槽——不重排 KV 的
名次（那是 Cron 按 EWMA 算出来的）。

### 内置兜底列表

`server/src/proxyip.js` 的 `FALLBACK_RELAY_HOSTS`，9 条 CMLiussss 域名型条目
（HK / JP / KR / DE / Aliyun / Oracle / DigitalOcean / Vultr / Multacom）。

硬编的是"从哪拿列表"，列表本身由 Cron 拉取后写进 KV（见下节）。兜底列表的职责只是"源不可达
时仍有得试"——社区源是个人维护的公益服务，说死就死。

另有 `server/src/cron.js` 的 `BUILTIN_RELAYS`（6 条 CMLiussss），那是 **Cron 探测池**的兜底：
Cron 拉源失败时拿它去测，测出来的排名再写进 KV。

### KV 池格式

`cron.js` 写进 `proxyip:top` 的是 JSON 数组，元素是对象：

```json
[{"host":"ProxyIP.HK.CMLiussss.net","port":443,"type":"http-connect","ms":182,"score":1274.0}]
```

读取端是 `race.js` 的 `parseRelayEntries()`，宽容解析以下三种都行：

```
# JSON 数组 或 { "relays": [...] }，元素是 "host[:port]" 或 { "host", "port" }
["ProxyIP.US.CMLiussss.net:443", "1.2.3.4:443"]

# 也接受订阅源原格式：纯文本逐行，# 之后是注释
104.16.1.1:443
ProxyIP.US.CMLiussss.net:443
```

解析不出来就当空池，由兜底列表接手——坏 JSON 不该让出口层整体不可用。

## 竞速

`server/src/race.js` 的 `startRace()`。

| 参数 | 默认 | 环境变量 |
|---|---|---|
| 槽位数 | 6 | `RACE_SLOTS` |
| 单槽超时 | 1500 ms | `RACE_SLOT_TIMEOUT_MS` |
| 全局超时 | 3000 ms | `RACE_GLOBAL_TIMEOUT_MS` |
| 交错启动间隔 | 120 ms | `RACE_STAGGER_MS` |
| KV 取前 N 个 | 4 | `RACE_KV_TOP` |

环境变量值非法或 ≤0 一律回落到默认（写错一个字符不该变成 0ms）。

要点：

- 槽位**全是中继候选**（NAT64 砍掉后不再有交错的两类）。
- 单槽超时直接传进 `connectViaProxyIP`，让超时回收发生在出站连接自己手里——在赛道上丢
  一个 Promise 只能丢引用，关不掉连接。
- 任一槽成功即回收其余槽位；赢家已定后才到达的隧道立即 `close()`，不留悬挂出站 socket。
- 全失败返回 `{ error: "all proxyip exits failed" }`，Session DO 回 `STATUS 0x03`。

## 健康记忆

`proxyip.js` 内存里的 Map，`"host:port" → { ok, ms, at }`，TTL 10 分钟
（`RELAY_HEALTH_TTL_MS`），上限 256 条。

- 每次 `connectViaProxyIP` 无论成败都 `noteRelay()`。
- TTL 的意义：硬编兜底列表必然随时间失效，10 分钟的探活记忆足以把刚失败过的中继踢到候选
  尾部，又不至于长期锚定一个刚变坏的。
- 这是优化不是状态：丢了顶多多试一次。

## 自建中继

公共中继的出口 IP 被 Cloudflare 系站点拉黑是常态，动态列表 + 竞速 + 亲和记忆是**自愈
机制，不是根治**。要根治只能自建：

1. 找一台不在 Cloudflare 网段内的主机（VPS 即可，出口 IP 干净）；
2. 在它上面跑一个只接受 `CONNECT host:port` 的 HTTP 代理，转发到目标，只认 443；
3. 把 `host:443` 加进 Cron 的中继池源，让它随 `proxyip:top` 进 KV；
4. 务必做访问控制——一个开放的 CONNECT 代理会被当成开放代理扫描滥用。

自建条目会和其他候选一起进入竞速，命中后由 Router DO 记住（`egress_type = http-connect`），
之后同一目标直接复用。

## 相关

- 出口选路的完整流程：[architecture.md](architecture.md) 第 3 节
- 配额与实测数据：[limitations.md](limitations.md)
- M0 实测：[m0-findings.md](m0-findings.md)
