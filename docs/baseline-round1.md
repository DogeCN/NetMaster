# 性能基线（第一轮）

日期：2026-10-04。机器：本机（192.168.1.13，大陆家宽）。部署：`proxy.0xa.cc.cd`。
客户端：`--manual --tunnels 4`。

## ⚠️ 本轮最大的一条发现：基线之前，生产是坏的

跑基线之前，所有出口请求都是 502（curl 20s 超时、`live_test` 单 WS 与分摊并发都是
**0/20**）。根因不是吞吐，也不是中继池：

- 生产 Worker 停在 **2026-10-03 16:57** 部署的那一份；
- `e58d1be`（2026-10-04 01:16）修的 `openExit` 作用域逃逸 —— `const direct` 声明在
  `if` 块内、却被块外的竞速失败分支引用 —— **还没上线**；
- 那个 bug 命中"直连失败且竞速全灭"这个最常见的故障态：`openStream` 来不及发
  `STATUS_NOEXIT` 就 reject，客户端拿不到任何响应帧，只能干等 20s → 502。

重新部署（Version `ea22c0e1`）后同一个请求：200 / 0.41s。**所以本基线是修复后的第一份，
不是修复前的。**

顺带修掉：工作区 `scripts/deploy.sh` 被 `core.autocrlf=true` 塞成 252 行 CRLF，
在 Windows 的 bash 下 `set -euo pipefail\r` 报 `invalid option name`，脚本根本跑不起来
（`.gitattributes` 早声明 `*.sh text eol=lf`，blob 一直是 LF，只是工作区没归一化）。

## 稳态扇开（预热之后，同一进程）

同一目标 `https://www.google.com/`，20 路并发，三轮：

| 轮 | 成功 | P50 | P95 | max |
|---|---|---|---|---|
| 1 | 20/20 | 957ms | 2834ms | 2906ms |
| 2 | 20/20 | 1054ms | 2182ms | 2440ms |
| 3 | 20/20 | 2384ms | 3302ms | 3966ms |

**20 路并发能全过**，P50 在 1–2.4s。之前"单条公共 SNI 中继并发吞吐不足、资源密集页
10s 才过"的判断在这份数据里不复现——那条结论来自 v0.2.3 时期（未部署修复的代码 + 那个
openExit bug）。

## 冷启动首请求：波动极大，是下一个大头

| 进程 | 首请求 | 次请求 |
|---|---|---|
| 1 | 0.531s | 1.145s |
| 2 | 0.524s | 0.516s |
| 3 | 5.445s | 1.129s |

三次里两次 ~0.52s、一次 5.4s；另有一次观测到 21.2s。
`ready in` 是 4.6–8.9s（本次 4.615s，另一次 8.715s / 8.906s）。

## 抖动定位（第二轮追加，未完）

补测 5 个进程、每进程两请求，抖动**不是只有首请求**：

| 进程 | 首请求 | 次请求 |
|---|---|---|
| 1 | 8.04s | **21.02s** |
| 2 | 0.58s | 0.60s |
| 3 | 0.30s | 1.65s |
| 4 | 0.20s | 0.50s |
| 5 | 0.21s | **20.21s** |

慢的那两次都是 **20–21s**，正好等于 `mux.go` 的 `streamReadyTimeout = 20s`，且日志里
**没有** `tunnel failed` / `[route]` —— 即隧道是好的、拨号也过了，慢在"等开流响应帧"。
`streamReadyTimeout` 是这条路径上唯一的 20s 常量（`mux.go:56`）。

同一隧道连续 6 次（间隔 4s）则全部 0.20–0.34s —— 稳态极稳，慢是**偶发单条流**。

下一步要判定是服务端开流慢（新 Session DO 冷启动？）还是客户端选隧道的问题，唯一能
看到服务端各段的手段是 `PROFILE=1` 部署后读 KV 的 `profile:<target>:<minute>`
（DO 内 console 不可见，m0 E9）。

### ⚠️ 当时没做成：本机网络被 TLS 干扰

准备开 `PROFILE=1` 部署时，`wrangler deploy` 报 `fetch failed`；随后发现本机对
`api.cloudflare.com` 返回 `SEC_E_UNTRUSTED_ROOT`（证书链不受信任），`www.microsoft.com`
超时、`cloudflare-dns.com` 被 RST —— **本机的 HTTPS 在那段时间被中间人干扰**。
期间任何远端测量都不可信，因此停止采集，等网络恢复再补。

生产状态是安全的：`deploy.sh` 有 `set -euo pipefail`，失败发生在 `wrangler deploy`
那一步、**在写 PROFILE 之前**，所以生产仍是上一版（含 `e58d1be` 的 openExit 修复，
不带 PROFILE 变量）。

## ECH（2026-10-04 复测，与 10-03 的"完全不生效"相反）

- DNS HTTPS RR 发布 `ech=`（71 字节）；
- `go run ./cmd/echprobe proxy.0xa.cc.cd`：**4/4 内层证书校验通过**（叶子证书
  `*.proxy.0xa.cc.cd`）；
- 明文对照：同一边缘 IP 上 SNI 写本域 **4/4 被 RST**，换 `www.cloudflare.com` 同 IP 200。

即 ECH 生效，且明文那条兜底路对本域是断的 —— ECH 是必需品，不是锦上添花
（`--no-ech` 一关很可能直接连不上）。

## 仍然不稳定的一项：`TestLiveCFHostedSuccessRate`

失败在**连边缘**这一步（`live dial failed: tls: ... forcibly closed`，
`ech also failed — exceeded 2s budget`），不是出口层。同一进程连续跑时会连续失败，
单独跑能 PASS；冷却 45s 也不总能恢复。样本不足以定位，标记为：
**CF-hosted 项的失败点在"拨号到边缘"，且 ECH 2s 预算经常不够** —— 下一轮优先查它。

## 下一轮怎么比

- 客户端：`NETMASTER_PROFILE=<path> netmaster serve` 落 JSON，用
  `node tools/profile.mjs compare a.json b.json`（指纹不同会 exit 2，拒绝比错）。
- 服务端：`PROFILE=1` 部署，KV 里读 `profile:<target>:<minute>`（TTL 1h）。
- 判据固定为：稳态 20 路并发的 P50/P95 + 首请求耗时。别拿单次 curl 当结论。
