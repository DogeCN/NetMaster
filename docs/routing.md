# 分流

分流在客户端，`client/internal/rules`。规则集是第三方每日构建的 Clash RULE-SET，
我们只决定"这份列表是直连类还是代理类"，**匹配顺序写死在 `Router.Match` 里**，不让
规则自带优先级互相覆盖。

## 匹配顺序

`Router.Match(host, port)` 的顺序（PRD §6.3）：

```
① 私网/回环/保留网段（privateCIDRs）      → direct，优先于一切规则
② 域名精确   exact                        host == value
③ 域名后缀   suffix                       host 以 .value 结尾（含自身）
④ 关键字     keyword                      host 含 value 子串
⑤ GeoIP(CN)                               解析后落在 CN 网段 → geoAction（默认 direct）
⑥ 端口       port                         "25" 或 "1000-2000"
⑦ 兜底       fallback                     默认 proxy
```

要点：

- **第 ① 段最高优先级**：服务端 `connect()` 禁连私网，送过去必然失败。`localhost` 也在这
  一段（按回环处理）。IPv6 的 `::1/128`、`fc00::/7`、`fe80::/10` 在内。
- **IP 字面量与域名走不同的分支**：CIDR 只对"输入本来就是 IP 字面量"生效。域名不做预解析
  （域名原样传给服务端），归属判断交给 GeoIP 阶段。
- **后缀匹配只切在 `.` 边界上**：`notexample.com` 不会命中 `example.com`。
- **端口规则在 `port == 0` 时不参与**（调用方没解析出端口，例如 SOCKS5 的某些情形）。
- **GeoIP 阶段的动作可由规则改**：`geoip-cn` 规则只用来改 `geoAction`（末条生效），默认
  CN → direct。

## 动作只有两个

`direct` / `proxy`，**没有 `auto`**。主机不在任何列表里时由 GeoIP 判断归属，判断不出来就
走兜底动作（默认 `proxy`）——不需要第三种状态。

## 规则集来源

内置四份 Loyalsoldier/clash-rules（`release` 分支，每日构建），各自对应一个动作：

| 列表 | 动作 |
|---|---|
| `private.txt` | direct |
| `direct.txt` | direct |
| `proxy.txt` | proxy |
| `gfw.txt` | proxy |

装载顺序（`rules.LoadRules`）：并行拉取（总预算 3 秒，单源超时 3 秒）→ 解析合并 → 成功则写
磁盘缓存 → 全失败读缓存 → 缓存也没有就用内置兜底集。来源由 `Router.Source()` 给出：
`fetched` / `cache` / `builtin`。

- 部分源挂掉时，缺的那几份用缓存补——不因为一次抖动丢掉一整类规则。
- 预算到期就用已到手的部分：一个慢源不该让启动卡住。
- 源偶发返回 HTML 错误页时按内容再判一次（`looksLikeRuleset`），不会把一页 HTML 当规则集
  塞进缓存——那样"成功"了却一条规则都没有。
- 第三方列表里混着 GEOSITE、PROCESS-NAME、MATCH 等不支持的类型是常态，认不出来的行跳过并
  计数，数量由 `SkippedLines()` 暴露在启动日志里。因为一行不认识就起不来是不可接受的。

内置兜底集只有 `private` + `gfw` 两份，故意做小：它是"完全没网络时的最低可用"，不是完整
列表。

## 自定义规则

```bash
netmaster serve --rules /path/to/rules.txt
```

或在 config.json 里写 `"rules": "/path/to/rules.txt"`。

格式是 **Clash RULE-SET**（不是 netmaster 原生格式）：

```
DOMAIN,example.com
DOMAIN-SUFFIX,google.com
DOMAIN-KEYWORD,google
IP-CIDR,10.0.0.0/8
IP-CIDR6,::1/128
```

认这几种：`DOMAIN`（精确）、`DOMAIN-SUFFIX`（后缀）、`DOMAIN-KEYWORD`（关键字）、
`IP-CIDR` / `IP-CIDR6`（网段，`no-resolve` 尾注忽略）。

动作按**文件名**推断：路径含 `direct` 视为直连，否则按 `proxy`（安全侧）。给了坏路径或空
列表会直接报错退出——用户明确指定了自己的规则集，静默降级成内置源比看得见的失败更糟。

`--rules` 非空时**内置源整个不拉**：再叠一层只会让"为什么这个域名走了代理"变得没法回答。

## GeoIP

CN 网段表从 `https://ispip.clang.cn/all_cn.txt` 拉取并落盘缓存（约 4300 条 CIDR，TTL 7 天），
后台加载不阻塞启动。**没有表时一律返回 `known=false`**，调用方退回兜底动作，不会因为拉取
失败影响可用性。

表只有 IPv4，纯 IPv6 站点一律按"非 CN"处理。

## "TCP 能连"不等于"这个站能直连"

分流只回答"这个站该直连还是代理"，回答不了"**这条直连此刻通不通**"：GFW 常常放行 TCP
握手，直到看见 TLS ClientHello 里的明文 SNI 才发 RST（或静默丢弃）。而 CONNECT 一旦回了
200 就等于提交了选择，浏览器随后只会报连接失败，我们却拿不到任何"该回退"的信号。

所以 `internal/proxy/replay.go` 把判定推迟到第一个数据包回来之后，按代价从低到高依次
尝试（`relayWithReplay`）。**规则直连的域名也在这条阶梯里**——旧语义"用户规则失败就该
失败"只对 TCP 拨号失败成立（站点真挂了，改道也救不了，现在仍然直接报错）；对 SNI 被拦
不成立：用户写 direct 的意思是"这个站通常该直连"，不是"被拦了也给我报错"。

| 级 | 路径 | 什么时候进这一级 | 首包窗口 |
|---|---|---|---|
| ① | 直连首段 | 一切直连（规则直连**明文**起步；分流判 proxy 的域名**带分片**起步） | 3s（`relayProbeWait`） |
| ② | 分片重试 | 仅当①**明文**起步且被拦：重拨一条直连、带分片重写首段 | 3s |
| ③ | 代理隧道 | 仅当②也失败（或①本来就带分片） | —（沿用已有 mux） |

**`--no-frag`（config `no-frag`）关掉整条①直连赌注与②分片重试**：分流判 proxy 的目标
直接走隧道，规则直连的目标被拦后直接改道。它存在的原因是这条阶梯的**盲区**——判据只有
传输层（首字节有没有回来），看不见应用层语义：站点 WAF 按来源 IP 拒绝直连时回的是合法
403，阶梯会当成"直连成立"，既不回退还把 `fragDirect` 记 6 小时。实测例子（2026-10-06，
arena.ai）：本机直连 403（CF-RAY …-SEA），经香港中继 200（…-HKG）。这类站点只有
"别赌直连"一条解。

起步形态由 `dial()` 的 `directMode` 决定（`exitProxy` / `directPlain` / `directFrag`）：
规则直连（geoip CN、用户 direct 规则）明文起步——CN 站点明文大多能通，先付 400ms 分片
延迟是浪费；分流判 proxy 的域名带分片起步——用户装这个软件本身就说明明文 SNI 大概率被拦。
分片记忆（`fragDirect`）可以把明文起步升级成分片起步，跳过注定失败的明文尝试。

三级共用同一套判定（2026-10-04 起的有意反转）：

- **有回应** → 成立，把探到的首包交给客户端，转入普通转发；
- **超时无回应 → 按阻断处理**：被墙网络里"TCP 连上、ClientHello 被静默丢弃"远比"慢站点"
  常见（BBC/Wikipedia 明文直连都是 20s 无响应而非 RST），把它当"慢"的后果是请求留在一条
  永远不会回应的直连上，用户干等自己的超时（实测 25s+）。代价：首字节真的超过 3s 的目标
  被推去代理并记 30 分钟——多绕一跳远好过整页挂死；
- **零字节就被断开** → 同样判阻断，降到下一级，把缓存的首段数据重放进去。

### ②分片直连（TLS-RF）是穿过去，不是绕过去

②排在③之前是这条阶梯的核心：分片成功就**留在直连**，不多绕一跳、也不在服务端多烧一次
`connect()` 子请求（免费版 50 子请求/invocation 是硬顶）。原理见 `client/internal/tlsfrag`：
阻断设备靠**重组**读 SNI，把 ClientHello 首段切成 8B 片、8ms 间隔（只分前 400 字节，
SNI 必在其中；剩下的字节与"能不能读到 SNI"无关，分片只会白付延迟），重组窗口先到期，
它看到的是一堆不完整的碎片。可选的 OOB 变体（`--frag-oob` / config `frag-oob`）把第一个
分片用 TCP 紧急数据（MSG_OOB）发出，对付"分片被重新聚拢"的中间盒；紧急字节是否进入对端
字节流取决于 SO_OOBINLINE，所以默认关闭。

②必须**重新拨**一条连接：触发它的那次直连已被对端 RST，同一个 socket 上重发只会得到
同样的 RST。

### 记忆层：别每次都重付试错

| 记忆 | 含义 | TTL | 触发 |
|---|---|---|---|
| `fragDirect` | 这个域名分片直连可行 | 6h | 阶梯②成功（或带分片起步的①成功） |
| `directBlocked` | 这个域名该走代理 | 30min | 代理隧道真的送出字节后（`NoteProxyConfirmed`） |
| `directDown` | 这个域名直连在 TCP 层就失败 | 5min | 规则直连拨号失败（`NoteDirectDown`） |

- `fragDirect` 优先级**高于** `directBlocked`：分片已验证可行，不该被"明文挨过一次拦"的
  冷却压掉。②失败时**必须忘掉**它——过期的分片记忆会让此后每条连接白付 400ms 再落代理，
  一次失败的探测按 TTL 持续计费；
- `directDown` 是短负记忆：TCP 都连不上（站点挂了/本机网络对该目标异常）不值得每个请求
  重付一次完整拨号超时，5 分钟内直接落代理。它不改变失败的结局——该 502 还是 502。
  三张记忆表的过期条目由 `SweepExpired` 在周期探活的节拍里清扫（惰性删除兜底）。

### 惊群挡板

首屏是几十条并发连接打同一个域名，而一次探测要 5s TCP 赌注 + 3s 首段窗口——没有标记的话
它们会**各自**完整付一遍。`Server.probing`（`sync.Map`）让第一个请求认领探测权，其余请求
直接落代理；探测结论出来后由记忆层接管后续请求。

### 直连超时与兜底

建连超时 5s（`DialTimeout`，健康站点 TCP 建连 1s 内完成；15s 的老默认只服务"对死目标保持
耐心"，那现在是 `directDown` 的职责）。池子全死（按 `Alive()` 计，不是 `Len()`）且
`DirectFallback` 开启时退回尝试性直连——这正是阶梯存在的原因。TCP 拨号失败时若近 5 分钟
失败过（`directDown`），跳过直连直接落代理。

浏览器全程无感，只看到握手慢了一点。

代理侧有一半对称的逻辑（`relayWithProxyReplay`）：隧道建立 ≠ 这跳真能用。Worker 首次见到
一个 CF 承载的目标时要内联选中继，可能先踩到"TCP 能通但不干活"的中继；此时 200 已经回了，
TLS 握手期隧道死亡，浏览器只能看到一个莫名的安全错误。做法同样是缓存首段 + 零字节即断时
换出口重放一次。

两侧都只对 **443** 启用：HTTPS 首段（ClientHello）自包含、且 SNI 阻断正发生在这里；其他
协议可能"客户端先发一半再等回应"，探测窗口会白白多等 3 秒。

注意措辞：能确定的只是"这条路径没能把这段数据送出去"，原因可能是 RST、连接被关闭，也可能
是 Worker 侧出站连接失败。日志不会一律归为 "blocked by RST"。

## 相关

- 服务端出口选路：[architecture.md](architecture.md)
- 排障：[troubleshooting.md](troubleshooting.md)
