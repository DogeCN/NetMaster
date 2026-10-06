# NetMaster 客户端

本地 HTTP/SOCKS5 代理，负责决定每个请求从哪儿出去。Go，标准库之外只有
websocket 一个依赖。

```bash
go build -o netmaster.exe ./cmd/netmaster
```bash
netmaster serve --server <域名> --password <PASSWORD>
```

首次启动（不传 flag 时）交互式询问服务地址与口令，写进
`%AppData%/netmaster/netmaster.json`——一份文件存所有持久化数据：配置、口令、
节点优选缓存。启动后监听端口自动选择（先试 8080/1080，被占顺延），接管系统代理并指向
选定端口，退出时自动还原；被强杀时由看门狗兜底还原（行为落在同目录的 `watchdog`
日志里）。`--manual` 只打印地址不接管；`--no-frag` 关掉分片直连（站点 WAF 按来源 IP 拒绝直连
时用，如 arena.ai）；`--local` 纯绕过模式（直连/分片/ECH 出口，不建隧道、不需要凭据）；
`--reset` 清除全部持久化数据。

配置来源优先级：命令行 flag > 持久化文件 > 默认值。字段与 serve 的 flag 一一对应
（server / password / manual / rules / tunnels / no-ech / insecure / frag-oob / no-frag），
另有 `entry_cache` 块（节点优选缓存）。详见 [../docs/operations.md](../docs/operations.md)。

## 命令

| 命令 | 用途 |
|---|---|
| `serve` | 日常唯一命令（`--local` 纯绕过模式） |
| `nodes` | 持续发请求，观察自适应选路的实时效果（`--ipcheck` 统计出口 IP 分布） |
| `restore` | 崩溃/强杀后手动还原系统代理 |
| `--reset` | 清除全部持久化数据 |

`watchdog` 是 serve 派生的内部命令，不面向用户。`cmd/echprobe` 是 ECH 握手级
诊断工具（`go run ./cmd/echprobe <域名>`）。

## 结构

```
cmd/netmaster      子命令入口与全部 flag 定义
cmd/echprobe       ECH 诊断：取配置 → 带校验握手 → 不校验握手 → 明文对照

internal/cache     进程间共享的磁盘缓存（原子写，带年龄）
internal/config    持久化单文件（appdata）：配置 + 口令 + 节点优选缓存
internal/instlock  serve 单实例锁（命名互斥体）
internal/profile   分段耗时采集（NETMASTER_PROFILE=1 时开）
internal/proto     协议 v2 帧编解码（与服务端 protocol.js 互为镜像）
internal/entry     入口候选：服务端域名 DNS（DoH）+ 优选缓存 + 社区优选源
internal/geoip     CN 网段表：拉取、缓存、按域名判断是否落在国内
internal/outbound  内部协议客户端：auth 握手、mux 会话、ECH 出站（RetryConfig 自愈）
internal/proxy     HTTP / SOCKS5 服务端；直连降级阶梯（明文→分片→代理）与失败重放
internal/rules     分流：内置 Clash 规则集、geoip 阶段、匹配顺序
internal/selector  节点池、出口阶梯记忆（fragDirect/directBlocked/directDown）、探活
internal/tlsfrag   TLS-RF 分片与 OOB 变体
internal/tlsutil   TLS / ECH 原语、DoH 取 ECH 配置、ECH 短路
internal/sysproxy  Windows 系统代理接管与还原
internal/procwait  等待进程退出（看门狗用）
```

## 设计要点

改动前先读：

- [../docs/routing.md](../docs/routing.md) —— 分流依据、出口优先级、CONNECT 失败重放
- [../docs/relay.md](../docs/relay.md) —— 中继为什么决定 200 还是 403

几条容易踩的：

- **出口选择是"学一次就锁"**。直连出口是本机宽带 IP、代理出口是 CF/中继 IP，来回切会掉
  登录态。所以绑定粘住，只在明确失败时改选（directBlocked 30min / directDown 5min）。
- **"TCP 能连"不代表"没被墙"**。CONNECT 隧道会把选择推迟到首个数据包回来之后再定，
  被阻断就改走代理并重放。别把 `dial` 成功当成可用性结论。
- **入口候选的"成员资格"和"优先级"是两回事**。社区源决定谁进候选，本机探测决定先试谁。
  别在服务端测延迟 —— 快慢是"你的网络到那个 IP"的属性。
- **Go 的 typed-nil 陷阱**。`DialWS()` 返回具体指针 `*WSConn`，把它装进 `net.Conn` 接口
  字段后接口不为 nil，`conn != nil` 判断会通过并 panic。存接口字段时注意用 `err == nil` 判空，
  或直接存具体类型。

## 测试

```bash
go vet ./...
go test ./...            # outbound 的集成测试需本地 devserver，默认跳过

# 端到端（Go 客户端 ↔ 真实 forward.js）
cd ../server && node test/devserver.mjs 8799 &
go test ./internal/outbound -run TestMuxE2E -v
```
