# NetMaster 服务端

Cloudflare Worker：终止内部协议 over WebSocket，解 mux 帧，出站连接目标。
目标是 Cloudflare 承载的站点时借第三方中继出站。整个 HTTP 面只有 WS 升级
一条路，没有任何端点。

```bash
npm install          # 只有测试需要（ws）
node build.mjs       # src/ -> _worker.js
npm test
```

**`_worker.js` 是产物，不要手改。**

## 结构

```
src/index.js           fetch 入口：只把 WebSocket 升级交给 Forwarder
src/protocol.js        内部协议：会话开帧、mux 帧编解码
src/forward.js         会话转发、mux、出站目标选择、中继亲和、动态中继列表
src/crypto.js          MD5、auth 派生、常量时间比较
src/relayprobe.js      问中继"这个域名经你会返回什么"
src/util.js            withTimeout 等小工具
```

`build.mjs` 把 `src/` 按顺序拼接成单个 `_worker.js`（剥掉 import/export，模块顶层名字
唯一）。加新模块要同时加进 `build.mjs` 的 `order`。

## 协议

见 [../docs/architecture.md](../docs/architecture.md)。一句话：WS 首条消息是
16 字节 auth（md5(PASSWORD)），之后全是 mux 帧（`[idLen][sessionId][payload]`），
控制帧（`0x00` 开头）负责告知会话就绪与失败原因。

## 设计要点

- **worker 不能连 Cloudflare 自己的 IP**。目标是 CF 承载的站点时必须借中继，多一跳。
- **中继的出口 IP 决定 200 还是 403**。所以选择中继不靠随机：先问候选"这个域名经你会返回
  什么"，403 淘汰；学到的绑定持久化到 D1（`relay_binding`），新 isolate 直接继承。
- **中继列表动态获取**（社区源 + 内置兜底），硬编的是"从哪拿列表"而不是列表本身。
- **探测只能用 `fetch` + `cf.resolveOverride`**，且必须有能力自检并如实上报——否则
  "平台不能探测"和"中继全被封"会长得一模一样。
- **探测在用户首个请求的内联路径上**，必须快速失败：单次 2.5s、单轮 3s 预算。
- **探测用注入的 `fetcher.fetch`**，拿不到就报"cannot probe"，不回退全局 `fetch`——不同作用域
  会逃出本请求的出口身份，也会让测试真的联网。

## 测试

| 文件 | 覆盖 |
|---|---|
| `crypto.mjs` | MD5 已知向量、auth 派生、常量时间比较 |
| `protocol.mjs` | 会话开帧解析/构造、mux 帧 |
| `integration.mjs` | auth 握手、多会话复用一条 WS、交错数据 |
| `control.mjs` | 出站失败时必须回控制帧告知客户端原因 |
| `relay.mjs` | 中继源解析、候选顺序、亲和、`relays: []` 禁用 |
| `devserver.mjs` | 真实 forward.js 的本地 WS 服务，供 Go 端到端测试 |

细节见 [../docs/relay.md](../docs/relay.md)。
