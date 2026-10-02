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
//   KV        Cron 健康结果（proxyip:top）。缺席时竞速只用内置兜底列表。

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

  // Cron 触发器（PRD §9，每小时刷新 KV 健康结果）在 M5 实现。这里只挂住处理器，
  // 免得 wrangler.toml 里的 triggers 指向一个不存在的 handler。
  async scheduled(event, env) {
    if (String(env.DEBUG || "") === "1") {
      console.log(`[cron] fired at ${event?.scheduledTime ?? "?"} (M5 placeholder)`);
    }
  },
};