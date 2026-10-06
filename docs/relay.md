# 中继（ProxyIP）

## 为什么需要中继

Workers 的 `connect()` 不能拨 Cloudflare 自有网段（`server/src/exits.js` 的 `CF_V4` /
`CF_V6` 会提前拒绝这类目标，省掉一次注定失败的连接）。而大量站点托管在 Cloudflare 上，
于是这些目标只剩一条出路：借一个第三方 SNI 中继出站。

```
浏览器 ──▶ netmaster ──▶ CF 边缘(Worker / Session DO) ──▶ 中继 ──▶ 目标站
                                                          按 SNI 转发
```

**这一跳的出口 IP 决定了 Cloudflare 系站点给你 200 还是 403**——站点看到的是中继，不是你。

隧道里跑的是客户端到目标的原始字节。目标是 HTTPS 时，TLS 由客户端端到端完成，中继只
搬运密文：SNI 型中继读 ClientHello 里的 SNI 决定转发去处，不碰明文。

### 关于 NAT64

PRD v1.1 给 NAT64 安排的角色正是绕开上面这条限制。但 M0 E3 实测：Workers `connect()`
对 IPv6 字面量与 NAT64 合成地址**一律立即失败**（<2ms，连拨号都没发生），
level66 / well-known / Trex / nat64.net 四个前缀、多个目标全部如此，IPv4 对照组 4ms 成功。
不是前缀选择问题，是运行时不支持 IPv6 出站。**NAT64 出口已从架构中移除**，CF 托管目标的
出口只剩中继一类。详见 [m0-findings.md](m0-findings.md)。

## 中继条目

- 类型：公共中继全是 `sni`（TLS ClientHello 路由）；自建 VPS 中继是 `http-connect`
  （先 HTTP CONNECT 再转发）。类型决定 `dialRelay` 的握手方式。
- 格式：`host[:port]`，端口缺省 **443**（`RELAY_PORT`）。中继语义就是反代 CF 的 443，所以
  只收 443 的条目。
- 解析：`parseRelay()` 取最后一个冒号，后面是 1–5 位数字才算端口，否则整串当主机名——
  这样裸 IPv6 字面量不会被误拆。

## 候选来源：部署工作流测速

候选列表**硬编在部署工作流**里（`deploy.yml` 的 `RELAYS`，全部 9 个 CMLiussss 域名型
候选），部署时由 `server/tools/probe-relays.mjs` 逐条做真实 TLS 握手探测——证书校验
通过才算可用（盲转发/自签证书的候选当场现形），按延迟排序写进 KV 的 `proxyip:top`
（默认取前 `RELAY_TOP_N=6` 条）。

部署是**唯一**写这个键的时机。为什么这样设计：

- 候选表放 Worker 里会过时，而 Worker 无法自测（免费版没有 Cron，子请求预算也不允许
  对每个目标都探测一遍）；
- 定时任务已被取缔：KV 写配额被打爆过一次（2026-10-06，见 limitations.md），
  部署是低频、可控、天然带"部署即验证"语义的写入时机。

实测参考（2026-10-06，GH runner 视角）：9 条候选可用 3 条——Oracle / KR / JP；
其余分别死于握手 alert（按连接轮换后端的"薛定谔中继"，`PROBE_TIMES=2` 也未必筛得干净）、
证书过期、DNS 不存在、超时。ipdb 裸 IP 源 10/10 全灭（盲转发），已弃用。

### KV 池格式

`proxyip:top` 是 JSON 数组：

```json
[{"host":"ProxyIP.Oracle.CMLiussss.net","port":443,"type":"sni","ms":5975}]
```

`type` 是中继的握手方式；`ms` 是部署时的探测延迟，仅供参考。读取端是 `order.js` 的
`parseRelayEntries()`，宽容解析三种格式（JSON 数组 / `{"relays":[...]}` / 纯文本逐行）；
解析不出来当空池——客户端会看到 `0x03`（no proxyip candidate available）。

## 顺序拨号

`server/src/order.js` 的 `dialOrdered()`。候选按序**逐个**试，不再竞速：

| 参数 | 默认 | 环境变量 |
|---|---|---|
| 候选上限 | 6 | `ORDER_SLOTS` |
| 单条尝试上限 | 5000 ms | `ORDER_SLOT_TIMEOUT_MS` |

为什么放弃竞速：竞速的收益是"新目标首建连最快"，代价是最坏 6 条并发出站 socket +
等量子请求预算。候选池缩到个位数、且顺序本身带记忆之后，第一个候选几乎总是上次的
赢家——并发换来的首包时间抵不过多烧的预算。顺序执行的代价是"首候选死了要逐个等下去"，
由单条 5s 时限兜底（跨洋 SNI 中继握手实测 2-3s，5s 是留了余量的）。

要点：

- **成功提前**：第 i>0 条成功 → 移到首位；
- **失败置后**：失败 → 踢到末尾，继续试下一条。每条候选**只试一次**（没有尝试计数的话，
  全灭场景会永远转圈——实现当场踩到的死循环）；
- **顺序变了才写回 KV**：由 Session 的 `persistRelayOrder` 执行，**每会话预算 3 次**
  （`RELAY_ORDER_WRITE_BUDGET`）——一次首屏可能重排几十次，不设上限的话 KV 写配额
  就是这么被打爆的。预算花完后内存照常维护，只是不落盘：下一个会话从稍旧的顺序起步；
- 全失败返回 `{ error: "all N proxyip exits failed" }`，Session DO 回 `STATUS 0x03`。

## 会话内亲和

Session DO 的 egress 缓存（LRU 512）：目标 → 中继的映射，**只在本 WebSocket 连接内
有效**。命中直接拨上次的中继，不进候选序列；3 秒内没有目标首字节即证伪删除。中继顺序
的会话内存副本同样在首用时从 KV 读一次，之后就地维护。

## 自建中继

公共中继的出口 IP 被 Cloudflare 系站点拉黑是常态，部署测速 + 顺序记忆是**自愈机制，
不是根治**。要根治只能自建：

1. 找一台不在 Cloudflare 网段内的主机（VPS 即可，出口 IP 干净）；
2. 在它上面跑一个只接受 `CONNECT host:port` 的 HTTP 代理，转发到目标，只认 443；
3. 把 `host:443` 加进 `deploy.yml` 的 `RELAYS` 列表，下次部署起参与测速排序；
4. 务必做访问控制——一个开放的 CONNECT 代理会被当成开放代理扫描滥用。

自建条目是 `http-connect` 型，CONNECT 2xx 即端到端验证，会与公共中继一起参与顺序拨号。

## 相关

- 出口选路的完整流程：[architecture.md](architecture.md) 第 3 节
- 配额与实测数据：[limitations.md](limitations.md)
- M0 实测：[m0-findings.md](m0-findings.md)
