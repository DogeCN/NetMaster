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

  // Cron（PRD §9）：每小时刷新 KV 里的出口健康排名。分批探测 + last-good-wins，
  // 细节见 cron.js。workerHost 用于剔除回指自身的 ProxyIP。
  async scheduled(event, env, ctx) {
    if (!env.KV) {
      console.log("[cron] skipped: KV binding missing");
      return;
    }
    let workerHost = "";
    try {
      workerHost = new URL(event?.request?.url || "https://workers.dev/").hostname;
    } catch {}
    try {
      const stats = await runCron(env, ctx, workerHost);
      console.log(`[cron] ${JSON.stringify(stats)}`);
    } catch (e) {
      // Cron 失败不重试：下一轮自然会重来，保留上一轮 KV 结果。
      console.error(`[cron] failed: ${e.message || e}`);
    }
  },
};