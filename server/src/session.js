// SessionDO —— 每条 WebSocket 连接一个（PRD §7.1）。职责：
//   首帧认证（HMAC + TS 窗口 + 流 ID 校验）→ 流表管理 → 开帧/数据帧/控制帧分发
//   → 出口选路（直连优先，失败转 ProxyIP：会话缓存 → Router DO → 竞速，PRD §7.2）
//   → 每流响应帧与 CLOSE。
//
// Hibernation：WS 经 acceptWebSocket 注册，空闲时 DO 休眠、连接保持；客户端
// 协议层 Ping 在边缘自动应答，不唤醒 DO。唤醒后实例内存是空的——流表随休眠
// 一起消失，而休眠的前提是没有挂起的出站 socket（M0 实测），所以丢失的只可能是
// 早已没有流量的流：客户端在死流上下一条数据会收到 0x01 + CLOSE，自行回收。
//
// 背压（PRD §4.7）：客户端→目标方向每流缓冲上限 1 MiB，超过即 CLOSE 该流，
// 不影响其他流。

import { connectViaProxyIP, parseRelay, RELAY_TYPE_HTTP_CONNECT } from './proxyip.js';
import { startRace } from './race.js';
import { targetHash, routerName, routerShardId } from './router.js';

const STREAM_BUFFER_LIMIT = 1024 * 1024;

// PENDING_FRAME_LIMIT 是"首帧认证未完成期间"允许排队的字节上限。客户端一个页面
// 几十条连接也用不到 1 MiB；超了说明客户端不遵守协议（或者在打首帧风暴），断开。
const PENDING_FRAME_LIMIT = 1024 * 1024;

// 连接建立后多久还没收到目标的首字节，就判定这条出口路径是死的（PRD §7.2）。
// 中继回了 200 不代表目标可达——它只代表中继愿意转发。
const FIRST_BYTE_GRACE_MS = 3000;

// 会话级出口缓存上限：只为省掉同连接内的重复查询，不是状态。
const EGRESS_CACHE_MAX = 512;

class SessionDO {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.password = String(env.PASSWORD || "");
    this.ws = null;
    this.authed = false;
    this.authPending = false; // 首帧认证进行中
    this.pendingFrames = []; // 认证期间到达的帧，按序排队
    this.pendingBytes = 0;
    this.streams = new Map(); // id -> { writer, wq, wqBytes, writing }
    this.closedIds = new Set(); // 近期已关流：迟到数据帧按原样丢弃，不误解析成开帧
    this.egress = new Map(); // target_hash -> { host, port }：本连接内的出口亲和
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
    if (!this.authed) {
      // 认证是 async（HMAC 计算会让出执行权），紧跟着首帧到达的开帧会看到
      // authed 仍为 false —— 必须在第一个 await 之前同步占位，否则会被误判成
      // 第二个首帧。客户端"发首帧后立刻并发开流"是常态（一个页面几十条连接），
      // 所以这些帧要排队等认证结果，而不是丢弃：丢弃会让客户端只能等到 20s
      // 超时才知道自己失败了。
      if (this.authPending) {
        this.pendingFrames.push(data);
        this.pendingBytes += data.byteLength;
        if (this.pendingBytes > PENDING_FRAME_LIMIT) {
          this.log("too many frames before auth completed");
          try { ws.close(1008, ""); } catch {}
        }
        return;
      }
      this.authPending = true;
      return this.handleFirstFrame(ws, data);
    }
    return this.handleFrame(ws, data);
  }

  // drainPending 按到达顺序处理认证期间排队的帧。
  async drainPending() {
    const queued = this.pendingFrames;
    this.pendingFrames = [];
    this.pendingBytes = 0;
    for (const data of queued) {
      if (!this.authed) return; // 认证失败：连接已被关闭
      await this.handleFrame(this.ws, data);
    }
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
    this.authPending = false;
    this.log(`authenticated; stream ${f.streamId} -> ${f.host}:${f.port}`);
    await this.openStream(f.streamId, f.host, f.port);
    await this.drainPending();
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

  // ---- 出口选路（PRD §7.2） ----

  // openExit 直连优先：不需要多付一跳的目标永远不多付。直连失败本身就是"目标在
  // Cloudflare 网段或不可达"的信号（connect() 拨 CF 段必被平台拒），这时才走
  // ProxyIP：会话缓存 → Router DO → 竞速。
  async openExit(atyp, host, port) {
    const direct = await directConnect(atyp, host, port);
    if (!direct.error) return { socket: direct.socket };
    // 出口失败的归因是排障刚需，不受 DEBUG 门控（tail 里必现）。
    console.error(`[exit] direct ${host}:${port} failed: ${direct.error}`);
    // tail 对 Session DO 的 console 输出不可见（实测），KV 是可靠的诊断通道
    this.env.KV?.put("debug:lastExit", `${new Date().toISOString()} ${host}:${port} direct: ${direct.error}`).catch?.(() => {});
    this.log(`direct exit failed (${direct.error}); trying proxyip`);
    // 没部署出口层（无 ROUTER/KV 绑定）时保持纯直连语义：直连失败就是失败。
    if (!this.env.ROUTER && !this.env.KV) return { error: direct.error };

    const hash = await targetHash(host);

    // ① 会话级缓存：只在本 WebSocket 会话内有效。
    const mem = this.egress.get(hash);
    if (mem) {
      const r = await connectViaProxyIP(mem.host, mem.port, host, port);
      if (!r.error) return { socket: r.socket, hash, relay: mem };
      this.log(`cached relay ${mem.host}:${mem.port} failed: ${r.error}`);
      this.egress.delete(hash);
    }

    // ② Router DO：跨会话复用的唯一来源。
    const hit = await this.routerLookup(hash);
    if (hit) {
      const r = await connectViaProxyIP(hit.host, hit.port, host, port);
      if (!r.error) {
        this.rememberEgress(hash, hit);
        this.learn(hash, hit.type || RELAY_TYPE_HTTP_CONNECT, `${hit.host}:${hit.port}`);
        return { socket: r.socket, hash, relay: hit };
      }
      this.log(`router relay ${hit.host}:${hit.port} failed: ${r.error}`);
      this.forget(hash); // 映射被证伪：立刻删，别让下一个流再踩同一脚
    }

    // ③ 竞速：候选 = KV top4 → 内置兜底补齐 6 槽（Router 映射刚被证伪或本就没有）。
    const race = await startRace({ env: this.env, log: (m) => this.log(m) }, { host, port });
    if (race.error) {
      console.error(`[exit] race ${host}:${port} all slots failed: ${race.error}`);
      this.env.KV?.put("debug:lastExit", `${new Date().toISOString()} ${host}:${port} race: ${race.error} | direct: ${direct.error}`).catch?.(() => {});
      return { error: `${direct.error}; ${race.error}` };
    }
    const relay = parseRelay(race.relay);
    this.rememberEgress(hash, relay);
    this.learn(hash, RELAY_TYPE_HTTP_CONNECT, race.relay);
    return { socket: race.socket, hash, relay };
  }

  routerStub(hash) {
    const r = this.env.ROUTER;
    if (!r) return null;
    return r.get(r.idFromName(routerName(routerShardId(hash))));
  }

  // routerLookup 命中返回 { host, port, type }。绑定缺席、请求出错、条目过期都
  // 当未命中：映射只是缓存，缺了顶多多一次竞速。
  async routerLookup(hash) {
    const stub = this.routerStub(hash);
    if (!stub) return null;
    try {
      const res = await stub.fetch(`https://router/lookup?hash=${hash}`);
      if (!res.ok) return null;
      const row = await res.json();
      const relay = parseRelay(row?.id);
      return relay ? { host: relay.host, port: relay.port, type: row.type } : null;
    } catch (e) {
      this.log(`router lookup failed: ${e.message || e}`);
      return null;
    }
  }

  // learn / forget 是异步旁路：不在出站连接的临界路径上，失败也无妨。
  learn(hash, type, id) {
    this.routerWrite(hash, "/learn", { hash, type, id });
  }

  forget(hash) {
    this.routerWrite(hash, "/forget", { hash });
  }

  routerWrite(hash, path, body) {
    const stub = this.routerStub(hash);
    if (!stub) return;
    stub
      .fetch(`https://router${path}`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(body),
      })
      .catch((e) => this.log(`router ${path} failed: ${e.message || e}`));
  }

  rememberEgress(hash, relay) {
    this.egress.delete(hash); // 重插以刷新 LRU 顺序
    this.egress.set(hash, { host: relay.host, port: relay.port });
    if (this.egress.size > EGRESS_CACHE_MAX) {
      this.egress.delete(this.egress.keys().next().value);
    }
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
    const ex = await this.openExit(atyp, host, port);
    if (ex.error) {
      this.log(`stream ${id} connect failed: ${ex.error}`);
      this.send(encodeResponse(id, STATUS_NOEXIT));
      this.send(encodeCloseControl(id));
      this.rememberClosed(id);
      return;
    }
    const rec = {
      socket: ex.socket,
      ws: this.ws,
      writer: ex.socket.writable.getWriter(),
      wq: [],
      wqBytes: 0,
      writing: false,
      hash: ex.hash || null,
      firstByteTimer: null,
    };
    if (rec.hash) {
      // 出口学到的映射先乐观记下，3 秒内没有首字节就承认学错了。
      rec.firstByteTimer = setTimeout(() => {
        rec.firstByteTimer = null;
        this.log(`stream ${id} no first byte in ${FIRST_BYTE_GRACE_MS}ms, forgetting route`);
        this.egress.delete(rec.hash);
        this.forget(rec.hash);
      }, FIRST_BYTE_GRACE_MS);
    }
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
        if (rec.firstByteTimer) {
          // 首字节到了：出口路径成立，撤掉 3 秒失效定时器。
          clearTimeout(rec.firstByteTimer);
          rec.firstByteTimer = null;
        }
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
      if (rec.firstByteTimer) clearTimeout(rec.firstByteTimer);
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
    this.authPending = false;
    this.pendingFrames = [];
    this.pendingBytes = 0;
    this.ws = null;
  }
}
