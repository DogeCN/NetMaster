# NetMaster

自建代理客户端 + Cloudflare Worker 服务端。目标：把"分流选路"和"最后一跳的连通性"
这两件一直靠手调的事变成自动的。

```
浏览器 ──▶ netmaster (127.0.0.1:8080 / :1080)
              │
              ├─ 国内站 ──────────────▶ 目标站            直连，2 跳
              │
              └─ 境外站 ──▶ 内部协议 over WS ──▶ Cloudflare 边缘(worker) ──▶ 目标站
                                                            └──▶ [中继] ──▶ 目标站   4 跳
```

## 组成

| 目录 | 内容 |
|---|---|
| `client/` | Go 客户端：本地 HTTP/SOCKS5 代理、分流、入口候选池、ECH |
| `server/` | Cloudflare Worker：自有协议 mux 转发、中继动态获取与亲和 |
| `docs/` | 架构、分流、中继、运维、排障 |
| `scripts/` | 构建脚本 |

## 快速开始

### 1. 服务端

整个部署只有三步，全部在 GitHub 上完成：

**① 配三个 Secret**（`gh secret set` 或仓库 Settings 页面）：

| Secret | 用途 |
|---|---|
| `CLOUDFLARE_API_TOKEN` | CI 调 Cloudflare API（需 Workers Scripts / D1 编辑权限） |
| `CLOUDFLARE_ACCOUNT_ID` | 账号 ID |
| `PASSWORD` | 客户端连接口令，CI 透传成 Worker Secret |

**② push 到 main**。CI 自动：跑测试 → 建同名 D1 → 应用 schema → 部署 Worker →
把 PASSWORD 传给 Worker。CI 绿了就是部署完了，没有部署后验证 —— runner 的
网络环境和用户差别很大，健康与否由"客户端能不能连上"直接回答。

**③ 绑你自己的域名**。这是你自己的事，项目不管可达性：在 Cloudflare 控制台给
Worker 加一个 custom domain（会同时建 DNS 和路由），或自己用 wrangler 绑。

### 2. 客户端

```bash
cd client
go build -o netmaster.exe ./cmd/netmaster     # Windows；Linux/macOS 去掉 .exe
./netmaster.exe serve --server <你的域名> --password <PASSWORD>
```

配置来源优先级：**命令行 flag > config.json > 默认值**。写一份 config.json（当前
目录或 `%AppData%/netmaster/config.json`）之后，日常就是裸的 `netmaster serve`：

```json
{ "server": "<你的域名>", "password": "<PASSWORD>" }
```

启动后监听端口自动选择（先试 8080/1080，被占则顺延），系统代理自动指向选定
端口，退出时自动还原；进程被强杀时由看门狗兜底还原。ready 日志之后会补一行
"隧道建立成功（经节点 x）"—— 部署是否健康，这一行就是最直接的回答。

客户端配置就这两个值。没有 UUID 要生成、没有数据库 ID 要复制、没有订阅地址要
复制粘贴 —— 口令两端各自本地派生成 16 字节 auth，网络上不出现明文。

## 关键设计

这几条是踩过坑之后定下来的，改动前先读 [docs/](docs/) 里对应章节。

- **两端都是我们的，协议也是。** WS 之上只有一种帧：mux 帧（一条连接跑多个
  会话）+ 控制帧（告知失败原因）。鉴权是连接级的一次 16 字节比对，会话级零
  开销。没有 VLESS、没有 UUID、没有任何 HTTP 端点。
- **分流不靠域名穷举。** 规则表只强制指定"必须代理"和"必须直连"，其余交给 IP
  归属判断（`internal/geoip`）。
- **出口选择是学出来的，不是猜出来的。** 每个域名第一次连接后记住走哪条出口，
  之后粘住；只在明确失败时才改选。学到的绑定**落盘**，重启不丢。
- **"TCP 连通"不等于"这个站能直连"。** CONNECT 隧道把选择推迟到首个数据包
  回来之后再定，被阻断就改走代理并重放（`internal/proxy/replay.go`）。
- **中继的出口 IP 决定 Cloudflare 站给 200 还是 403。** 中继列表从社区源动态
  获取（硬编的是"从哪拿列表"而不是列表本身），服务端主动问候选中继"这个域名
  经你会返回什么"，学到的绑定存 D1，新 isolate 直接继承。
- **入口候选两个来源。** 服务端域名自己的 DNS 解析（永远可用）+ 社区优选 IP
  源（每次启动更新、逐源容错）。快慢由客户端本机探测决定，别人测的不算数。
- **启动即就绪。** 探测后台跑；社区源拉取有界等待（3 秒），失败退缓存。

## 已知限制

- 公共中继的出口 IP 被 Cloudflare 系站点拉黑是常态，动态列表 + 逐主机探测 +
  亲和持久化是自愈机制，不是根治；要根治只能自建中继（见 [docs/relay.md](docs/relay.md)）。
- ECH 在部分网络下会间歇失败（服务端返回 outer 名证书），客户端有 60s 短路 +
  2s 尝试预算兜底。个别站点既无 ECH 又被 IP+SNI 双拦，客户端无法本地绕过。
- geoip 的 CN 网段表只有 IPv4；纯 IPv6 站点一律按非 CN 处理（走代理）。

## 开发

```bash
# 客户端
cd client && go build ./... && go vet ./... && go test ./...

# 服务端
cd server && npm install && node build.mjs && npm test

# 端到端（Go 客户端 ↔ 真实 forward.js）
cd server && node test/devserver.mjs 8799 &
cd client && go test ./internal/outbound -run TestMuxE2E -v
```
