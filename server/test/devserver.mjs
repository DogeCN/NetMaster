// devserver.mjs — 协议 v2 的 Node 对端实现，本地/CI E2E 用。
// 与 src/session.js 同一套帧格式与状态机，出站用 node:net（Workers 侧是
// cloudflare:sockets）。差异点：denyLoopback=false 可放开回环目标（生产禁回环，
// 测试要连本地 echo 服务器），connectTimeoutMs 可调小（快速测 0x03 路径）。
import net from "node:net";
import http from "node:http";
import { WebSocketServer } from "ws";
import { authCode, tsWithinWindow, safeEqualBytes } from "../src/crypto.js";
import {
  parseFirstFrame, parseOpenFrame, isControl, parseCloseControl, streamIdFromBytes,
  encodeResponse, encodeCloseControl, encodeDataFrame,
  validStreamId, MAX_PAYLOAD,
  STATUS_OK, STATUS_BAD, STATUS_FORBIDDEN, STATUS_NOEXIT,
  ATYP_IPV4, ATYP_IPV6, ATYP_DOMAIN,
} from "../src/protocol.js";
import { isForbidden } from "../src/exits.js";

export function startDevserver(opts = {}) {
  const password = opts.password ?? "devserver-password";
  const denyLoopback = opts.denyLoopback ?? true;
  const connectTimeoutMs = opts.connectTimeoutMs ?? 5000;
  const bufferLimit = opts.bufferLimit ?? 1024 * 1024;

  const state = {
    password, denyLoopback, connectTimeoutMs, bufferLimit,
    authed: false,
    ws: null,
    streams: new Map(),
    closedIds: new Set(),
  };

  const send = (ws, bytes) => ws.send(bytes);
  const forbidden = (atyp, host, port) => {
    if (!state.denyLoopback && (host === "127.0.0.1" || host === "::1")) return false;
    return isForbidden(atyp, host, port);
  };

  // 每条流记录自己的 ws：连接切换后，旧连接的迟到事件不得发给新连接。
  const closeStream = (id, notify) => {
    const rec = state.streams.get(id);
    state.streams.delete(id);
    state.closedIds.add(id);
    if (rec) rec.socket.destroy();
    if (notify && rec?.ws) send(rec.ws, encodeCloseControl(id));
  };

  const connectOutbound = (host, port) =>
    new Promise((resolve) => {
      const socket = net.connect({ host, port });
      const timer = setTimeout(() => {
        socket.destroy();
        resolve({ error: "connect timeout" });
      }, state.connectTimeoutMs);
      socket.once("connect", () => { clearTimeout(timer); resolve({ socket }); });
      socket.once("error", (e) => { clearTimeout(timer); resolve({ error: e.code || e.message }); });
    });

  async function openStream(id, host, port) {
    if (process.env.DBG) console.log(`  srv openStream ${id} ${host}:${port}`);
    if (state.streams.has(id)) {
      send(state.ws, encodeResponse(id, STATUS_BAD));
      return;
    }
    const atyp = host.includes(":") ? ATYP_IPV6 : /^\d+\.\d+\.\d+\.\d+$/.test(host) ? ATYP_IPV4 : ATYP_DOMAIN;
    if (forbidden(atyp, host, port)) {
      send(state.ws, encodeResponse(id, STATUS_FORBIDDEN));
      send(state.ws, encodeCloseControl(id));
      state.closedIds.add(id);
      return;
    }
    const r = await connectOutbound(host, port);
    if (r.error) {
      send(state.ws, encodeResponse(id, STATUS_NOEXIT));
      send(state.ws, encodeCloseControl(id));
      state.closedIds.add(id);
      return;
    }
    const rec = { socket: r.socket, ws: state.ws, wq: [], wqBytes: 0, writing: false };
    state.streams.set(id, rec);
    send(state.ws, encodeResponse(id, STATUS_OK));

    rec.socket.on("data", (chunk) => {
      for (let off = 0; off < chunk.length; off += MAX_PAYLOAD) {
        send(rec.ws, encodeDataFrame(id, chunk.subarray(off, Math.min(off + MAX_PAYLOAD, chunk.length))));
      }
    });
    rec.socket.on("close", () => closeStream(id, true));
    rec.socket.on("error", () => closeStream(id, true));
  }

  function streamWrite(id, rec, payload) {
    rec.wq.push(payload);
    rec.wqBytes += payload.length;
    if (rec.wqBytes > state.bufferLimit) {
      closeStream(id, true);
      return;
    }
    if (!rec.writing) {
      rec.writing = true;
      (async () => {
        while (rec.wq.length > 0) {
          const chunk = rec.wq.shift();
          rec.wqBytes -= chunk.length;
          if (!rec.socket.destroyed && !rec.socket.writableEnded) {
            await new Promise((res) => rec.socket.write(chunk, res));
          }
        }
        rec.writing = false;
      })().catch(() => closeStream(id, true));
    }
  }

  async function handleMessage(ws, message) {
    const data = new Uint8Array(message);
    if (process.env.DBG) console.log('  srv<-', Buffer.from(data).toString('hex').slice(0, 32), 'authed:', state.authed);
    if (!state.authed) {
      const f = parseFirstFrame(data);
      if (!f || !validStreamId(f.streamId) || !tsWithinWindow(f.ts, Math.floor(Date.now() / 1000))) {
        if (f) send(ws, encodeResponse(f.streamId, STATUS_BAD));
        ws.close(1008, "");
        return;
      }
      const expect = await authCode(state.password, f.signed);
      if (!safeEqualBytes(expect, data.slice(0, 16))) {
        send(ws, encodeResponse(f.streamId, STATUS_BAD));
        ws.close(1008, "");
        return;
      }
      state.authed = true;
      await openStream(f.streamId, f.host, f.port);
      return;
    }
    if (data.length < 4) return;
    if (isControl(data)) {
      const id = parseCloseControl(data);
      if (id !== null) closeStream(id, false);
      return;
    }
    const id = streamIdFromBytes(data);
    const rec = state.streams.get(id);
    if (rec) {
      const payload = data.slice(4);
      if (payload.length > MAX_PAYLOAD) {
        closeStream(id, true);
        return;
      }
      streamWrite(id, rec, payload);
      return;
    }
    if (state.closedIds.has(id)) return;
    const f = parseOpenFrame(data);
    if (!f || !validStreamId(f.streamId)) {
      send(ws, encodeResponse(id, STATUS_BAD));
      return;
    }
    await openStream(f.streamId, f.host, f.port);
  }

  const wss = new WebSocketServer({ noServer: true });
  const server = http.createServer((req, res) => {
    res.writeHead(404, { "content-length": 0 });
    res.end();
  });
  server.on("upgrade", (req, socket, head) => {
    const u = new URL(req.url, "http://x");
    if (u.pathname !== "/") {
      socket.end("HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\n\r\n");
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => wss.emit("connection", ws, req));
  });
  wss.on("connection", (ws) => {
    // 一条连接一个会话；旧连接已在关闭中时直接接管（上一连接的 close 事件
    // 可能晚于新连接到达）。
    if (state.ws && state.ws.readyState === state.ws.OPEN) { ws.close(1013, ""); return; }
    // 每条连接是独立会话：流表与 closedIds 都是连接级状态，必须重置。
    for (const [, rec] of state.streams) rec.socket.destroy();
    state.streams.clear();
    state.closedIds.clear();
    state.ws = ws;
    state.authed = false;
    ws.on("message", (msg, isBinary) => {
      if (!isBinary) { ws.close(1003, ""); return; }
      handleMessage(ws, msg).catch(() => ws.close(1011, ""));
    });
    ws.on("close", () => {
      if (state.ws !== ws) return; // 已被新连接接管
      for (const [id, rec] of state.streams) { rec.socket.destroy(); state.closedIds.add(id); }
      state.streams.clear();
      state.ws = null;
      state.authed = false;
    });
    ws.on("error", () => {});
  });

  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const port = server.address().port;
      resolve({
        port,
        url: `ws://127.0.0.1:${port}/`,
        close: () => {
          for (const [, rec] of state.streams) rec.socket.destroy();
          wss.close();
          server.close();
        },
      });
    });
  });
}
