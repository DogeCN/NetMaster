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

前提：一个 Cloudflare 账号、一个托管在 CF 的域名（客户端要连它）。全程只涉及
一个口令（PASSWORD）和一个域名，没有别的。

### 1. 服务端

**首选：不克隆仓库，直接部署。** 从 [Release](../../releases/latest) 下载
`_worker.js`、`wrangler.toml`、`schema.sql` 放进同一目录，然后：

```bash
npx wrangler login
npx wrangler d1 create netmaster                          # 把输出的 database_id 填进 wrangler.toml
npx wrangler d1 execute netmaster --remote --file=schema.sql
npx wrangler secret put PASSWORD                          # 你定的口令，客户端要用同一个
npx wrangler deploy
```

最后在 Cloudflare 控制台给这个 Worker 绑定你的域名（custom domain）——
这一步同时建 DNS 和路由，是客户端能连上它的全部前提。

**或者：克隆仓库走 CI。** 给仓库配三个 Secret（`CLOUDFLARE_API_TOKEN`、
`CLOUDFLARE_ACCOUNT_ID`、`PASSWORD`，用 `gh secret set` 或仓库设置页），push
到 main，CI 自动完成建库、schema、部署、透传 PASSWORD。打 `v*` tag 会额外
构建 Release 产物。

### 2. 客户端

从 [Release](../../releases/latest) 下载 `netmaster.exe`（或克隆仓库自己
`go build`）。写一份 `config.json` 放在 exe 旁边（或 `%AppData%/netmaster/config.json`）：

```json
{ "server": "<你的域名>", "password": "<PASSWORD>" }
```

然后：

```bash
./netmaster.exe serve        # 或直接双击 exe，等价
```

双击启动的细节：首次双击若没有 config.json，会在 exe 旁边生成一个模板，填好
两个值再点一次即可；任何启动错误都会等一次回车再关窗口，原因不会一闪而过。

启动后监听端口自动选择（先试 8080/1080，被占则顺延），系统代理自动指向选定
端口，退出时自动还原；直接关闭窗口等于强杀进程，由看门狗兜底还原。ready 日志
之后会补一行 "tunnel established via node x"—— 部署是否健康，这一行就是最直接
的回答。

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

# 发版：打 tag 即构建全部 Release 产物（.github/workflows/release.yml）
git tag v0.1.0 && git push --tags
```
