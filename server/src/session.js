// SessionDO —— 每条 WebSocket 连接一个（PRD §7.1）。职责：
//   首帧认证（HMAC + TS 窗口 + 流 ID 校验）→ 流表管理 → 开帧/数据帧/控制帧分发
//   → 直连出口 → 每流响应帧与 CLOSE。
//
// Hibernation：WS 经 acceptWebSocket 注册，空闲时 DO 休眠、连接保持；客户端
// 协议层 Ping 在边缘自动应答，不唤醒 DO。唤醒后实例内存是空的——流表随休眠
// 一起消失，而休眠的前提是没有挂起的出站 socket（M0 实测），所以丢失的只可能是
// 早已没有流量的流：客户端在死流上下一条数据会收到 0x01 + CLOSE，自行回收。
//
// 背压（PRD §4.7）：客户端→目标方向每流缓冲上限 1 MiB，超过即 CLOSE 该流，
// 不影响其他流。

const STREAM_BUFFER_LIMIT = 1024 * 1024;

class SessionDO {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.password = String(env.PASSWORD || "");
    this.ws = null;
    this.authed = false;
    this.streams = new Map(); // id -> { writer, wq, wqBytes, writing }
    this.closedIds = new Set(); // 近期已关流：迟到数据帧按原样丢弃，不误解析成开帧
    this.idleTimer = null;
    this.lastActivity = 0;
  }

  log(msg) {
    if (String(this.env.DEBUG || "") === "1") console.log(`[session] ${msg}`);
  }

  async fetch(request) {
    if ((request.headers.get("Upgrade") || "").toLowerCase() !== "websocket") {
      return new Response(null, { status: 404 });
    }
    const pair = new WebSocketPair();
    this.state.acceptWebSocket(pair[1]);
    this.ws = pair[1];
    this.touch();
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  async webSocketMessage(ws, message) {
    this.touch();
    if (this.ws === null) this.ws = ws; // Hibernation 唤醒：重新接管连接
    let data;
    try {
      data = new Uint8Array(message);
    } catch {
      return;
    }
    if (!this.authed) return this.handleFirstFrame(ws, data);
    return this.handleFrame(ws, data);
  }

  async webSocketClose() {
    this.closeAll();
  }

  async webSocketError() {
    this.closeAll();
  }

  touch() {
    this.lastActivity = Date.now();
    // 服务端判死仅在醒着时执行（PRD §4.6）：休眠会清掉这个定时器，正好是想要的语义。
    if (this.idleTimer) clearTimeout(this.idleTimer);
    this.idleTimer = setTimeout(() => {
      if (Date.now() - this.lastActivity >= 180000) {
        this.log("idle 180s, closing session");
        try { this.ws?.close(1000, ""); } catch {}
        this.closeAll();
      }
    }, 185000);
  }

  send(bytes) {
    try {
      this.ws.send(bytes);
      return true;
    } catch (e) {
      this.log(`ws send failed: ${e.message || e}`);
      try { this.ws.close(1011, ""); } catch {}
      return false;
    }
  }

  // ---- 首帧：认证 + 打开流 1 ----

  async handleFirstFrame(ws, data) {
    // 结构校验
    const f = parseFirstFrame(data);
    if (!f || !validStreamId(f.streamId) || !tsWithinWindow(f.ts, Math.floor(Date.now() / 1000))) {
      this.log("first frame rejected (structure/ts/stream-id)");
      if (f) this.send(encodeResponse(f.streamId, STATUS_BAD));
      try { ws.close(1008, ""); } catch {}
      return;
    }
    // 认证校验：签名区 = TS 起的全部字节
    const expect = await authCode(this.password, f.signed);
    if (!safeEqualBytes(expect, data.slice(0, 16))) {
      this.log("first frame rejected (auth)");
      this.send(encodeResponse(f.streamId, STATUS_BAD));
      try { ws.close(1008, ""); } catch {}
      return;
    }
    this.authed = true;
    this.log(`authenticated; stream ${f.streamId} -> ${f.host}:${f.port}`);
    await this.openStream(f.streamId, f.host, f.port);
  }

  // ---- 已认证后的帧分发 ----

  async handleFrame(ws, data) {
    if (data.length < 4) return;
    if (isControl(data)) {
      const id = parseCloseControl(data);
      if (id !== null) this.closeStream(id, false);
      return;
    }
    const id = streamIdFromBytes(data);
    const rec = this.streams.get(id);
    if (rec) {
      const payload = data.slice(4);
      if (payload.length > MAX_PAYLOAD) {
        // 协议违规：关闭该流（PRD §4.5），不影响其他流。
        this.log(`stream ${id} payload ${payload.length}B exceeds ${MAX_PAYLOAD}`);
        this.closeStream(id, true);
        return;
      }
      this.streamWrite(id, rec, payload);
      return;
    }
    if (this.closedIds.has(id)) return; // 已关流的迟到数据，静默丢弃
    // 未知流 = 开帧（客户端在 STATUS 0x00 前不发数据，见 protocol.js 头注释）
    const f = parseOpenFrame(data);
    if (!f || !validStreamId(f.streamId)) {
      this.log(`open frame rejected (bad format / stream id ${id})`);
      this.send(encodeResponse(id, STATUS_BAD));
      return;
    }
    await this.openStream(f.streamId, f.host, f.port);
  }

  // ---- 流生命周期 ----

  async openStream(id, host, port) {
    if (this.streams.has(id)) {
      this.send(encodeResponse(id, STATUS_BAD)); // 流 ID 重复
      return;
    }
    const atyp = host.includes(":") ? ATYP_IPV6 : /^(\d{1,3}\.){3}\d{1,3}$/.test(host) ? ATYP_IPV4 : ATYP_DOMAIN;
    if (isForbidden(atyp, host, port)) {
      this.log(`stream ${id} forbidden: ${host}:${port}`);
      this.send(encodeResponse(id, STATUS_FORBIDDEN));
      this.send(encodeCloseControl(id));
      this.rememberClosed(id);
      return;
    }
    const r = await directConnect(atyp, host, port);
    if (r.error) {
      this.log(`stream ${id} connect failed: ${r.error}`);
      this.send(encodeResponse(id, STATUS_NOEXIT));
      this.send(encodeCloseControl(id));
      this.rememberClosed(id);
      return;
    }
    const rec = { socket: r.socket, ws: this.ws, writer: r.socket.writable.getWriter(), wq: [], wqBytes: 0, writing: false };
    this.streams.set(id, rec);
    this.send(encodeResponse(id, STATUS_OK));
    this.pumpOutbound(id, rec);
  }

  // pumpOutbound 把出站 socket 的数据封帧发回客户端；EOF/错误投递 CLOSE。
  // 帧发往 rec.ws（该流所属连接），而非 this.ws —— 实例被复用时旧流的迟到事件
  // 不能串到新连接上。
  async pumpOutbound(id, rec) {
    const sendTo = (b) => { try { rec.ws.send(b); return true; } catch { return false; } };
    const reader = rec.socket.readable.getReader();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        let buf = value;
        while (buf.length > MAX_PAYLOAD) {
          if (!sendTo(encodeDataFrame(id, buf.slice(0, MAX_PAYLOAD)))) return;
          buf = buf.slice(MAX_PAYLOAD);
        }
        if (!sendTo(encodeDataFrame(id, buf))) return;
      }
      this.closeStream(id, true);
    } catch (e) {
      this.log(`stream ${id} outbound read error: ${e.message || e}`);
      this.closeStream(id, true);
    }
  }

  // streamWrite 把客户端数据排队写出；积压超限即弃流（背压上限）。
  streamWrite(id, rec, payload) {
    rec.wq.push(payload);
    rec.wqBytes += payload.length;
    if (rec.wqBytes > STREAM_BUFFER_LIMIT) {
      this.log(`stream ${id} buffer exceeded ${STREAM_BUFFER_LIMIT}`);
      this.closeStream(id, true);
      return;
    }
    if (!rec.writing) this.drainWrites(id, rec);
  }

  async drainWrites(id, rec) {
    rec.writing = true;
    try {
      while (rec.wq.length > 0) {
        const chunk = rec.wq.shift();
        rec.wqBytes -= chunk.length;
        await rec.writer.write(chunk);
      }
    } catch (e) {
      this.log(`stream ${id} outbound write error: ${e.message || e}`);
      this.closeStream(id, true);
    } finally {
      rec.writing = false;
    }
  }

  // closeStream 关闭一条流：socket、流表、（可选）通知客户端。
  closeStream(id, notify) {
    const rec = this.streams.get(id);
    this.streams.delete(id);
    this.rememberClosed(id);
    if (rec) {
      try { rec.socket.close(); } catch {}
    }
    if (notify && rec?.ws) {
      try { rec.ws.send(encodeCloseControl(id)); } catch {}
    }
  }

  rememberClosed(id) {
    if (this.closedIds.size > 4096) this.closedIds.clear();
    this.closedIds.add(id);
  }

  closeAll() {
    for (const [id, rec] of this.streams) {
      try { rec.socket.close(); } catch {}
    }
    this.streams.clear();
    if (this.idleTimer) clearTimeout(this.idleTimer);
    this.idleTimer = null;
    this.authed = false;
    this.ws = null;
  }
}
