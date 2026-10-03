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
握手，直到看见 TLS ClientHello 里的明文 SNI 才发 RST。而 CONNECT 一旦回了 200 就等于提交
了选择，浏览器随后只会报连接失败，我们却拿不到任何"该回退"的信号。

所以 `internal/proxy/replay.go` 把判定推迟到第一个数据包回来之后，并且**按代价从低到高**
依次尝试三级出口（`relayWithReplay`）：

| 级 | 路径 | 什么时候进这一级 | 探首包窗口 | 额外代价 |
|---|---|---|---|---|
| ① | 明文直连 | 分流判 proxy 但没有可用出口，退回尝试性直连 | 3s（`relayProbeWait`） | 无 |
| ② | 分片直连 | 仅当 ① **零字节即断** | 4s（`relayFragProbeWait`） | 重拨 + 约 400ms 分片 |
| ③ | 代理隧道 | 仅当 ② 也失败 | —（沿用已有 mux） | 多一跳，服务端烧一次 `connect()` |

三级共用同一套判定：

- **有回应** → 成立，把探到的首包交给客户端，转入普通转发；
- **超时无回应** → 目标只是慢，认账（不误伤正常站点，不据此改道）；
- **零字节就被断开** → 这条路不可用，降到下一级，把缓存的首段数据重放进去。

进入阶梯的入口是 `dial` 的 `tentativeDirect`：**规则明确判 direct 的（局域网、`.cn` 名单）
不算尝试性，也不进阶梯**——那是用户自己写的规则，失败就该失败，不由我们替他改道。
只有"规则判 proxy、而 `hasUsableExit()` 为假"才退回直连赌一把（`DirectFallback`，serve 里恒开），
而"有可用出口"的判据是**没被判死的节点数**而不是池子大小——池里躺着一批死节点时 `Len()`
照样 >0，直连兜底永远不会触发。

### ②分片直连（TLS-RF）是穿过去，不是绕过去

②排在③之前是这条阶梯的核心：分片成功就**留在直连**，不多绕一跳、也不在服务端多烧一次
`connect()` 子请求（免费版 50 子请求/invocation 是硬顶）。原理见 `client/internal/tlsfrag`：
阻断设备靠**重组**读 SNI，把 ClientHello 首段切成 8B 片、8ms 间隔（只分前 400 字节，
SNI 必在其中，剩下的字节与"能不能读到 SNI"无关，分片只会白付延迟），重组窗口先到期，
它看到的是一堆不完整的碎片。参数是包级 var（`Chunk`/`Delay`/`MaxSpan`），测试缩到毫秒级跑。

②必须**重新拨**一条连接：触发它的那次直连已被对端 RST，同一个 socket 上重发只会得到
同样的 RST。没有记忆层时也会**试一次**——成本是一次拨号加 400ms，成功了这次调用就把它
记住了（`statelessFragDirecter` 只是不留下跨连接的结论）。

### 两级记忆层：别每次都重付试错

| 记忆 | 含义 | TTL | 优先级 |
|---|---|---|---|
| `fragDirect` | 这个域名分片直连可行 | 6h | **高于** `directBlocked` |
| `directBlocked` | 这个域名该走代理 | 30min | — |

- ②成功 → 记 `fragDirect`，下次直连带着分片起步，跳过注定失败的①；
- ②失败（含"因为记忆才分片、这次没奏效"）→ **忘掉** `fragDirect`。这一步漏了的后果不是
  浪费一次：`fragDirect` 优先级压着 `directBlocked`，一条过期记忆会让此后 6 小时每条连接
  先白付 400ms 再落代理，同时把"这个域名该走代理"的结论盖掉——一次失败的探测按 TTL 持续
  计费；
- ③成功 → 记 `directBlocked`，30 分钟内不再试直连。

有记忆时①那次连接**本身就带分片**（`writeFlight` 按记忆分片，探窗仍是 3s），失败即直接落
③——不会为了②再拨一次，因为②已经在①里做过一遍了。上表的 4s 只属于 `tryFragmentedDirect`
重新拨出来的那条连接。

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
