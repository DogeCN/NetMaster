// index.js — Worker 入口。
//
// 整个 HTTP 面只有一件事：把 WebSocket 升级转交 Session DO。没有端点 ——
// 没有 /health、/sub、/relaycheck。协议见 protocol.js；部署是否健康由
// "客户端能不能连上"直接回答。非 WS 请求一律 404 空 body（PRD §3.2：无额外头，
// 避免指纹）。
//
// Env:
//   PASSWORD  必需。GitHub Secrets 配置，CI 透传成 Worker Secret。
//   DEBUG     '1' 打开结构化日志（wrangler tail 查看）。
//   SESSION   Session DO 绑定。
//   ROUTER    Router DO 绑定（目标 → 出口映射）。缺席时出口层降级为纯直连。
//   KV        出口健康排名（proxyip:top，由 GitHub Actions 的 refresh-relays
//             定时任务写入）。缺席或为空时竞速只用内置兜底列表。

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const upgrade = (request.headers.get("Upgrade") || "").toLowerCase();
    if (url.pathname !== "/" || upgrade !== "websocket") {
      return new Response(null, { status: 404 });
    }
    const id = env.SESSION.idFromName(crypto.randomUUID());
    return env.SESSION.get(id).fetch(request);
  },

  // 无 scheduled handler：Worker Cron 已移除（PRD 附录 A6），中继池刷新由
  // GitHub Actions 的 refresh-relays.yml 定时写 KV。留一个空壳反而危险 ——
  // DO 内 console 输出在 tail 上不可见（m0-findings.md E9），死代码坏了也看不见。
};