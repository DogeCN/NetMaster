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
};
