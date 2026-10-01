/**
 * index.js — Worker 入口。
 *
 * 整个 HTTP 面只有一件事：把 WebSocket 升级交给 Forwarder。没有任何端点 ——
 * 没有 /health、/sub、/relaycheck、/probe。客户端讲的是内部协议（见
 * protocol.js），部署是否健康由"客户端能不能连上"直接回答，不需要一个
 * 专门回答这个问题的 HTTP 页面。
 *
 * Env:
 *   PASSWORD  必需。GitHub Secrets 配置，CI 透传成 Worker Secret。
 *             鉴权 = md5(utf8(PASSWORD))，两端各自派生。
 *   DEBUG     '1' 打开日志。
 */

import { authBytes } from './crypto.js';
import { Forwarder } from './forward.js';

export default {
	async fetch(request, env, ctx) {
		const url = new URL(request.url);

		if (request.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
			if (url.pathname !== '/') return text('not found', 404);
			return text('WebSocket upgrade required', 426);
		}

		const password = String(env.PASSWORD || '');
		if (!password) return text('PASSWORD secret not configured', 503);
		if (url.pathname !== '/') return text('not found', 404);

		const debug = env.DEBUG === '1' || env.DEBUG === 'true';
		const log = debug ? (...a) => console.log(...a) : () => {};

		const pair = new WebSocketPair();
		const client = pair[0];
		const server = pair[1];
		// The server side of a WebSocketPair must be accepted before it will
		// emit message/close events. Without this the handshake still appears
		// to succeed but no data is ever received — which looks exactly like a
		// dead upstream. allowHalfOpen keeps reads alive after the peer closes
		// its write side; older runtimes reject the options object.
		try {
			server.accept({ allowHalfOpen: true });
		} catch (_) {
			server.accept();
		}
		let fwd;
		try {
			fwd = new Forwarder(request, server, authBytes(password), {
				log,
				db: env.DB,
				ctx,
			});
		} catch (err) {
			// Surface the reason instead of a bare 500 — the most likely cause
			// is an unavailable outbound-TCP API.
			return text(`forwarder init failed: ${err?.message || err}`, 500);
		}
		fwd.start();
		log(`[ws] open from ${request.headers.get('CF-Connecting-IP') || '?'}`);
		// Disable permessage-deflate. Without this the runtime may negotiate
		// the extension, and our framing (and every client's) assumes raw
		// uncompressed binary frames — compressed messages fail to parse.
		return new Response(null, {
			status: 101,
			webSocket: client,
			headers: { 'Sec-WebSocket-Extensions': '' },
		});
	},
};

function text(body, status = 200) {
	return new Response(body, { status, headers: { 'content-type': 'text/plain; charset=utf-8' } });
}
