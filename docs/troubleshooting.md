# 排障

## 客户端

### 启动慢 / 启动日志

启动日志带自报耗时，**以它为准**：

```
ready in 188ms. browse normally; Ctrl+C to stop and restore system proxy.
```

`[geoip] CN ranges ready: 2980 (cache)` 之后如果长时间没有 `[proxy] ... on`，看
`[sys] cleanup stale system proxy` 那一行——上次异常退出留下的状态需要清理。

> **测量提示**：用 bash 的 `netstat` / `/dev/tcp` 轮询测启动耗时，开销可达 500ms 级，会严重
> 高估（曾测出 590ms 而实际 11ms）。用应用自报的 `ready in`。
>
> **Go 测试会缓存结果**：重复测量同一测试必须加 `-count=1`，否则多次输出完全一致。

### 上网慢，但只有某些站点慢

先分流对不对：

```bash
netmaster nodes --target https://<站点>   # 观察该站点走哪条出口
```

再对比直连基线（不经代理）：

```bash
curl -s -o /dev/null -w "%{http_code} %{time_total}s\n" https://<站点>
```

差 4 倍以上通常是**国内站被误判成代理**。检查规则：未列出的域名走 `auto`，由 IP 归属决定；
`netmaster nodes` 可以确认入口候选正常。

### 日志里的 `[route]` 行

```
[route] <host> direct unusable (<原因>) — switched to proxy and replayed
[route] <host> direct unusable (<原因>), proxy retry failed: <错误>
```

表示 CONNECT 隧道建立后直连没能把客户端的首段数据送达，于是改走代理并重放。
**注意别一律当成防火墙阻断** —— 上游 worker 出站失败（目标是 Cloudflare 承载、worker 连不了
CF 自身 IP）也会走这条路径。括号里是真实原因。

同一站点反复出现这两行，说明它在两条路之间反复横跳。客户端有 30 分钟的直连禁用冷却来阻止
级联；如果还在出现，说明两侧都确实不通。

### 节点全部超时

`[probe] 0/N reachable` 且探测耗时接近 4s 的整数倍 → 是 TCP 连不上（不是 ECH、不是应用层）。
中继或边缘 IP 不可达，等一会儿重试；`netmaster nodes` 可以看每个节点的实时健康度。

### 端口冲突 / 系统代理指向不存在的端口

```bash
netmaster restore     # 手动还原
```

强杀过 netmaster 时看门狗通常会兜底，但看门狗本身被杀就兜不了了。

### ECH 相关

日志出现 `exceeded 2s budget` 表示 ECH 尝试超预算，已自动改走普通 TLS。ECH 在部分网络下会
间歇失败（服务端返回 outer 名证书），属预期行为，有 60s 短路兜底，不影响可用性。

## 服务端

### 判断 403 归因

Worker 没有 HTTP 端点，从日志看：`DEBUG=1` 部署后 `wrangler tail`，中继选择会打
`<host>: relay <relay> answers <status>` 或 `no relay answered cleanly [...]`。

| 日志 | 含义 |
|---|---|
| 部分中继 2xx/3xx | 探测能挑到好的，D1 会记住 |
| 某站全部 403 | **该站拒绝所有这些出口 IP**，换公共中继没用，自建中继 |
| 全部 0 | 中继连不上 |

### 首次请求慢几秒

正常：那一轮在探测中继（最多 4 个候选，总预算 3s）。D1 里学到结果后就不会再探。

### 控制帧与客户端报错

客户端可能看到 `nodepool: all N nodes failed`。这是客户端侧的汇总错误；服务端在出站失败时
会通过控制帧带上真实原因（这正是 `test/control.mjs` 保证的行为）。把 `DEBUG=1` 打开看服务端
日志能拿到具体原因。

## 已知限制

| 现象 | 原因 |
|---|---|
| 某个 CF 站稳定 403 | 它拉黑了公共中继的出口 IP。自建中继是唯一根治手段 |
| 某站直连超时、走代理才通 | 站点的地域/bot 策略，不是墙；分流会自动学到 |
| 首次访问某 CF 站点很慢 | 中继选择 + 探测预算，命中 D1 后恢复正常 |

## 深入

- 分流依据与优先级 → [routing.md](routing.md)
- 中继机制、403 诊断、自建 → [relay.md](relay.md)
- 架构与跳数 → [architecture.md](architecture.md)
