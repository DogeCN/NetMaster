# NetMaster 客户端

本地 HTTP/SOCKS5 代理，负责决定每个请求从哪儿出去。Go，标准库之外只有
websocket 一个依赖。

```bash
go build -o netmaster.exe ./cmd/netmaster
netmaster serve --server <域名> --password <PASSWORD>
```

启动后监听端口自动选择（先试 8080/1080，被占顺延），接管系统代理并指向选定
端口，退出时自动还原；被强杀时由看门狗兜底还原。`--manual` 只打印地址不接管。

配置来源优先级：命令行 flag > config.json > 默认值。config.json 与 serve 的
flag 一一对应（server / password / manual / rules），放在当前目录或
`%AppData%/netmaster/config.json`。详见 [../docs/operations.md](../docs/operations.md)。

## 命令

| 命令 | 用途 |
|---|---|
| `serve` | 日常唯一命令 |
| `nodes` | 持续发请求，观察自适应选路的实时效果（`--ipcheck` 统计出口 IP 分布） |
| `restore` | 崩溃/强杀后手动还原系统代理 |

`watchdog` 是 serve 派生的内部命令，不面向用户。

## 结构

```
cmd/netmaster      子命令入口与全部 flag 定义

internal/cache     进程间共享的磁盘缓存（原子写，带年龄）
internal/config    config.json 的查找与解析
internal/entry     入口候选：服务端域名 DNS + 社区优选源（多源并行、缓存兜底）
internal/geoip     CN 网段表：拉取、缓存、按域名判断是否落在国内
internal/route     分流规则（顺序匹配、首条命中）+ 内置规则表
internal/nodepool  入口池：延迟探测、评分、熔断、域名亲和、学到的状态落盘
internal/outbound  内部协议客户端：auth 握手、mux 会话、ECH 出站
internal/proxy     HTTP / SOCKS5 服务端，CONNECT 隧道与失败重放
internal/tlsutil   TLS / ECH 原语与 ECH 短路
internal/sysproxy  Windows 系统代理接管与还原
internal/procwait  等待进程退出（看门狗用）
```

## 设计要点

改动前先读：

- [../docs/routing.md](../docs/routing.md) —— 分流依据、出口优先级、CONNECT 失败重放
- [../docs/relay.md](../docs/relay.md) —— 中继为什么决定 200 还是 403

几条容易踩的：

- **出口选择是"学一次就锁"**。直连出口是本机宽带 IP、代理出口是 CF/中继 IP，来回切会掉
  登录态。所以绑定粘住，只在明确失败时改选。学到的绑定落盘在
  `%LOCALAPPDATA%/netmaster/state-*.json`，重启不丢。手动锁定（--pin）已删除：
  亲和机制覆盖了它的场景，手动锁定是退化。
- **"TCP 能连"不代表"没被墙"**。CONNECT 隧道会把选择推迟到首个数据包回来之后再定，
  被阻断就改走代理并重放。别把 `dial` 成功当成可用性结论。
- **入口候选的"成员资格"和"优先级"是两回事**。社区源决定谁进候选，本机探测决定先试谁。
  别在服务端测延迟 —— 快慢是"你的网络到那个 IP"的属性。
- **Go 的 typed-nil 陷阱**。`DialWS()` 返回具体指针 `*WSConn`，把它装进 `net.Conn` 接口
  字段后接口不为 nil，`conn != nil` 判断会通过并 panic。存接口字段时注意用 `err == nil` 判空，
  或直接存具体类型。相关代码在 `internal/nodepool/probe.go`。

## 测试

```bash
go vet ./...
go test ./...            # outbound 的集成测试需本地 devserver，默认跳过

# 端到端（Go 客户端 ↔ 真实 forward.js）
cd ../server && node test/devserver.mjs 8799 &
go test ./internal/outbound -run TestMuxE2E -v
```
