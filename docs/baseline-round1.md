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

三次里两次 ~0.52s、一次 5.4s；另有一次观测到 21.2s（未复现）。
`ready in` 是 4.6–8.9s（本次 4.615s，另一次 8.715s / 8.906s）。

首请求的抖动比稳态的绝对值更值得先动：稳态 P50 已经 ~1s，而首请求能在 0.5s 与 21s
之间跳，用户的体感差异几乎全在这里。（`NETMASTER_PROFILE=1` 的分段报告就是为定位
"到底哪一段"准备的，下一轮用它。）

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
