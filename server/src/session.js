// SessionDO —— 每条 WebSocket 连接一个（PRD §7.1）。职责：
//   首帧认证（HMAC + TS 窗口 + 流 ID 校验）→ 流表管理 → 开帧/数据帧/控制帧分发
//   → 出口选路（直连优先，失败转 ProxyIP：会话缓存 → 竞速，PRD §7.2）
//   → 每流响应帧与 CLOSE。
//
// Router DO 与 KV 的出口排名已砍（2026-10-06）：中继列表硬编三条实测幸存者
// （proxyip.js），跨会话记忆的收益撑不起 Router DO 的子请求与运维成本；会话内
// 的出口亲和由 egress 缓存承担。
//
// Hibernation：WS 经 acceptWebSocket 注册，空闲时 DO 休眠、连接保持；客户端
// 协议层 Ping 在边缘自动应答，不唤醒 DO。唤醒后实例内存是空的——流表随休眠
// 一起消失，而休眠的前提是没有挂起的出站 socket（M0 实测），所以丢失的只可能是
// 早已没有流量的流：客户端在死流上下一条数据会收到 0x01 + CLOSE，自行回收。
//
// 背压（PRD §4.7）：客户端→目标方向每流缓冲上限 1 MiB，超过即 CLOSE 该流，
// 不影响其他流。

import { dialRelay, parseRelay, RELAY_TYPE_SNI } from './proxyip.js';
import { dialOrdered, KV_RELAY_KEY } from './order.js';
// 名字直接用 profile.js 里的原名：**不要**写 `flush as flushProfile`。
// build.mjs 的拼接式打包只是把 import 语句整行删掉，不处理重命名 —— 别名在源码里
// 读着完全正常，符号却在 bundle 里不存在，表现为运行期 ReferenceError。
// build.mjs 现在会硬拒绝带 as 的相对 import，所以这个坑至少不会再无声发生。
import { makeProfiler, flush } from './profile.js';

// target_hash = 目标域名（小写）SHA-256 前 16 字节十六进制。只存哈希不存域名：
// egress 缓存是会话内的路由记忆，不是访问日志，没必要留可还原的目标名。
async function targetHash(host) {
  const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(String(host).toLowerCase()));
  return Array.from(new Uint8Array(d).slice(0, 16))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

const STREAM_BUFFER_LIMIT = 1024 * 1024;

// PENDING_FRAME_LIMIT 是"首帧认证未完成期间"允许排队的字节上限。客户端一个页面
// 几十条连接也用不到 1 MiB；超了说明客户端不遵守协议（或者在打首帧风暴），断开。
const PENDING_FRAME_LIMIT = 1024 * 1024;

// 连接建立后多久还没收到目标的首字节，就判定这条出口路径是死的（PRD §7.2）。
// 中继回了 200 不代表目标可达——它只代表中继愿意转发。
const FIRST_BYTE_GRACE_MS = 3000;

// 会话级出口缓存上限：只为省掉同连接内的重复查询，不是状态。
const EGRESS_CACHE_MAX = 512;

// RELAY_ORDER_WRITE_BUDGET 是本会话允许的"候选顺序写回 KV"次数上限。
// KV 写配额紧张（2026-10-06 被打爆过一次），而一次首屏可能重排几十次 ——
// 预算花完后内存照常维护，只是不落盘，下一会话从稍旧的顺序起步。
const RELAY_ORDER_WRITE_BUDGET = 3;

// CONNECT_BUDGET 是本会话一生允许的**子请求**数（耗尽后回收换新预算）。
//
// 单位是子请求而不是"建连次数"：DO 之间的 fetch 与 connect 同池计费（m0 E8 的
// 实测口径就是"fetch + connect"成对消耗），所以 Router DO 的 lookup/learn/forget
// 也必须计入，否则真实消耗会显著高于计数，优雅回收来不及救、平台硬失败先到。
//
// 取 20 而不是更高：一次"新目标"的典型开销是 1 次 lookup + 1~2 次 connect
// （竞速错峰，通常首个槽就赢）+ 1 次 learn ≈ 3~4 次子请求；20 的计数上限对应
// 最坏约 40 次，留 10 次余量给 DoH、KV 调试写入等零散开销，稳稳落在平台的
// 50 子请求/调用之内。
const CONNECT_BUDGET = 20;

// PROF_FLUSHES_PER_SESSION 是**全会话**的 KV 写次数上限（不是每个目标各算一份）。
// 见 profile.js 的 flush(budget)：一条连接会承载很多目标，按目标各算一份的话，
// 一次首屏就能把配额烧掉，而那正是最不该花的场景。
const PROF_FLUSHES_PER_SESSION = 12;

// PROF_FLUSH_EVERY_STREAMS 是"每开多少条流落一次盘"。
//
// 为什么需要它（2026-10-04 实测）：此前只在**关连接 / 撞预算回收**时 flush，于是
// 中等负载一条都落不下来 —— 每会话 9~12 次请求既够不到 CONNECT_BUDGET(20)，客户端
// 又在正常关闭之前被强杀，KV 里一条新记录都没有。而"坏窗口里那 20~40s 的尾巴"
// 恰恰只在这种中等负载下出现，采不到就等于没有观测。
//
// 刻意**不用定时器**：pending timer 会阻止 DO 休眠（m0 E4/E9），而观测手段不该改变
// 被观测对象 —— DO 驻留时长正是要省的东西。按流计数既不烧配额，也不依赖会话结束。
const PROF_FLUSH_EVERY_STREAMS = 8;

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
    this.directFailed = new Set(); // 直连被平台拒绝的目标（hash），本连接内不再重试
    // 中继候选的会话内存副本：首次需要时从 KV 读一次（dialOrdered 经 setter 回填），
    // 之后成功提前/失败置后就地维护；顺序变化经 persistRelayOrder 写回 KV。
    this.relayOrder = null;
    this.relayOrderWrites = 0; // 本会话已花掉的顺序写回预算
    this.connectCount = 0; // 本激活期已消耗的子请求数（connect 与 DO fetch 都计）
    this.idleTimer = null;
    this.draining = false; // 预算见底且仍有在途流：等它们跑完再回收
    this.lastActivity = 0;
    this.profBudget = { left: PROF_FLUSHES_PER_SESSION };
    this.profSession = makeProfiler(this.env, { target: "session" });
    this.profTargets = new Map(); // target -> 采集器（见 profFor）
    this.profFlush = null; // 最近一次落盘的 promise，供 webSocketClose 等待
    this.profStreams = 0; // 本会话开过的流数（按 PROF_FLUSH_EVERY_STREAMS 触发落盘）
  }

  // profFor 取（并缓存）某个目标的采集器。
  //
  // 为什么按目标分桶而不是整会话一个：客户端那边是按 host 记录的（profile 的
  // Fact/Fingerprint 都带目标），服务端共用一个 "session" 桶就对不上账了。
  // 协议首帧没有余量塞 trace id，"目标 + 时间窗" 是唯一的关联手段
  // （见 profile.js 头部）。
  profFor(target) {
    let p = this.profTargets.get(target);
    if (!p) {
      p = makeProfiler(this.env, { target });
      this.profTargets.set(target, p);
    }
    return p;
  }

  // flushProfile 把本会话所有采集器落盘。写 KV 要配额，所以只在会话结束时、以及
  // 每 PROF_FLUSH_EVERY_STREAMS 条流时做；失败一律吞掉 —— profile 是排障工具，
  // 它坏了不该影响转发。
  async flushProfile() {
    const all = [this.profSession, ...this.profTargets.values()];
    this.profTargets.clear();
    for (const p of all) {
      try {
        await flush(p, this.env, this.profBudget);
      } catch {
        // 见上：观测数据不值得为之抛错。
      }
    }
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
    await this.profFlush; // 等 KV 落盘：关连接是本会话最后一个能安全写的机会
  }

  async webSocketError() {
    this.closeAll();
    await this.profFlush;
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
    // 认证整段计时。用 try/finally 而不是逐个 return 前手动收尾 —— 这里有三条
    // 出口（结构拒、认证拒、正常），漏掉一条就意味着"失败的那几次"没有数据，
    // 而失败路径的耗时恰恰是要查的。
    const endAuth = this.profSession.begin("auth");
    try {
      // 结构校验
      const f = parseFirstFrame(data);
      if (!f || !validStreamId(f.streamId) || !tsWithinWindow(f.ts, Math.floor(Date.now() / 1000))) {
        this.log("first frame rejected (structure/ts/stream-id)");
        this.profSession.count("auth.reject", 1);
        if (f) {
          this.send(encodeResponse(f.streamId, STATUS_BAD));
        } else {
          // 结构层面就解析不出来，没有合法 stream id 可引用 —— 用保留的 0 号回 0x01，
          // 让客户端也能拿到 STATUS 而不是只看到裸 close（PRD §11 的"非法均回 0x01"）。
          this.send(encodeResponse(0, STATUS_BAD));
        }
        try { ws.close(1008, ""); } catch {}
        return;
      }
      // 认证校验：签名区 = TS 起的全部字节
      const expect = await authCode(this.password, f.signed);
      if (!safeEqualBytes(expect, data.slice(0, 16))) {
        this.log("first frame rejected (auth)");
        this.profSession.count("auth.reject", 1);
        this.send(encodeResponse(f.streamId, STATUS_BAD));
        try { ws.close(1008, ""); } catch {}
        return;
      }
      this.profSession.count("auth.ok", 1);
      this.authed = true;
      this.authPending = false;
      this.log(`authenticated; stream ${f.streamId} -> ${f.host}:${f.port}`);
      await this.openStream(f.streamId, f.host, f.port);
      await this.drainPending();
    } finally {
      endAuth();
    }
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
      this.send(encodeCloseControl(id)); // 0x01 后补 CLOSE：PRD §11 "静默关闭"的两段式
      return;
    }
    await this.openStream(f.streamId, f.host, f.port);
  }

  // ---- 出口选路（PRD §7.2） ----

  // openExit 直连优先：不需要多付一跳的目标永远不多付。直连失败本身就是"目标在
  // Cloudflare 网段或不可达"的信号（connect() 拨 CF 段必被平台拒），这时才走
  // ProxyIP：会话缓存 → Router DO → 竞速。
  async openExit(atyp, host, port) {
    const p = this.profFor(`${host}:${port}`);
    const hash = await targetHash(host);

    // direct 必须提到 if 外面声明。
    //
    // 它原本是块内 const，却被下面的竞速失败分支引用 —— 块级作用域，那两行必然
    // ReferenceError。而它命中的正是最常见的故障态（直连失败 **且** 竞速 6 槽全灭：
    // 中继池挂了、目标落在 CF 网段）。后果比抛异常更糟：openStream 还没来得及发
    // STATUS_NOEXIT 就 reject，客户端拿不到任何响应帧，只能干等 20s 超时；认证期间
    // 排队的开帧也一起丢掉。
    //
    // 之所以能上线：integration.mjs 断言了 0x03，但它连的是 devserver 这个**替身**，
    // 真实的 openExit 零覆盖；而 `node --check` 只查语法，查不出运行期未定义标识符。
    let direct = null;

    // 直连失败记忆：解析到 CF 网段的目标直连必被平台拒（每次失败还白烧子请求
    // 预算），本连接内直接走 ProxyIP。
    if (!this.directFailed.has(hash)) {
      const endDirect = p.begin("exit.direct");
      direct = await directConnect(atyp, host, port);
      endDirect();
      if (!direct.error) {
        p.count("exit.rung0");
        return { socket: direct.socket };
      }
      console.error(`[exit] direct ${host}:${port} failed: ${direct.error}`);
      this.directFailed.add(hash);
      this.log(`direct exit failed (${direct.error}); trying proxyip`);
    }
    // 没有出口层变量的历史条件已随 RouterDO/KV 读取一起移除：候选是硬编列表，
    // 竞速永远可用，直连失败就竞速。

    // ① 会话级缓存：只在本 WebSocket 会话内有效。
    const mem = this.egress.get(hash);
    if (mem) {
      const endCache = p.begin("exit.cache");
      const r = await dialRelay(mem, host, port);
      endCache();
      if (!r.error) {
        p.count("exit.rung1");
        return { socket: r.socket, hash, relay: mem };
      }
      this.log(`cached relay ${mem.host}:${mem.port} failed: ${r.error}`);
      this.egress.delete(hash);
    }

    // ② 顺序出口：候选顺序 = KV 里的部署测速结果，会话内就地维护
    // （成功提前、失败置后），顺序变化写回 KV（有预算，见 persistRelayOrder）。
    // 赢家进 egress 缓存，同目标后续流直接复用、不再进这里。
    const sess = this;
    const endOrder = p.begin("exit.order");
    const picked = await dialOrdered(
      {
        env: this.env,
        get order() { return sess.relayOrder; },
        set order(v) { sess.relayOrder = v; },
        log: (m) => sess.log(m),
        onReorder: (o) => sess.persistRelayOrder(o),
      },
      { host, port }
    );
    endOrder();
    if (picked.error) {
      console.error(`[exit] order ${host}:${port} exhausted: ${picked.error}`);
      // direct 为 null 是正常情形：本会话早前那条流已经把直连判死、directFailed
      // 记住了，于是这次根本没再拨。把它读成 "(已在本次会话失败)"，而不是再崩一次 ——
      // 上一版就是在这里崩的，而这里恰好是最常被走到的一行。
      const directErr = direct ? direct.error : "(already failed earlier this session)";
      return { error: `${directErr}; ${picked.error}` };
    }
    const relay = { ...parseRelay(picked.relay), type: picked.type || RELAY_TYPE_SNI };
    p.count("exit.rung2");
    this.rememberEgress(hash, relay);
    return { socket: picked.socket, hash, relay };
  }

  // persistRelayOrder 把重排后的候选顺序写回 KV。
  //
  // 只在顺序真的变了时被调（dialOrdered 的 onReorder），但**写回有预算**：一次
  // 页面加载能造出几十个新目标，极端情况下每个都可能重排一次 —— 不设上限的话
  // KV 写配额就是这么被打爆的（2026-10-06 的教训）。预算花完后内存照常更新，
  // 只是不再落盘：下一个会话从 KV 读到的是稍旧的顺序，代价只是多试几条。
  persistRelayOrder(order) {
    this.relayOrder = order;
    if (!this.env.KV || this.relayOrderWrites >= RELAY_ORDER_WRITE_BUDGET) return;
    this.relayOrderWrites++;
    const body = JSON.stringify(order.map((r) => ({ host: r.host, port: r.port, type: r.type || RELAY_TYPE_SNI })));
    this.env.KV.put(KV_RELAY_KEY, body).catch(() => {});
  }

  // charge 记一次子请求消耗（connect 与 DO fetch 同池）。
  charge(what) {
    this.connectCount++;
    if (this.connectCount >= CONNECT_BUDGET) {
      this.log(`subrequest budget ${this.connectCount} reached (last: ${what}), recycling session`);
    }
  }

  rememberEgress(hash, relay) {
    this.egress.delete(hash); // 重插以刷新 LRU 顺序
    // type 必须入缓存：mem 命中时 dialRelay 按 type 分发，丢了会把
    // http-connect 中继当 SNI 拨（握手方式不对，连不上）。
    this.egress.set(hash, { host: relay.host, port: relay.port, type: relay.type || RELAY_TYPE_SNI });
    if (this.egress.size > EGRESS_CACHE_MAX) {
      this.egress.delete(this.egress.keys().next().value);
    }
  }

  // ---- 流生命周期 ----

  async openStream(id, host, port) {
    if (this.streams.has(id)) {
      // 流 ID 重复：0x01 之后补 CLOSE，客户端的等待者才能完整收尾
      this.send(encodeResponse(id, STATUS_BAD));
      this.send(encodeCloseControl(id));
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
    const p = this.profFor(`${host}:${port}`);
    const endExit = p.begin("exit.total");
    const ex = await this.openExit(atyp, host, port);
    endExit();
    if (ex.error) {
      this.log(`stream ${id} connect failed: ${ex.error}`);
      this.send(encodeResponse(id, STATUS_NOEXIT));
      this.send(encodeCloseControl(id));
      this.rememberClosed(id);
      return;
    }
    // 预算管理：免费版每个 invocation 50 个子请求，每条流约 1 个（connect），
    // 加上 DNS 缓存未命中时的 DoH。预算见底前优雅断开，客户端会带着等待队列
    // 重连拿到全新预算 —— 这是有意的续命机制，不是故障。
    //
    // **排空而不是当场杀**：还有在途流时只标记，等最后一条流结束再关。资源密集
    // 页面（Netflix 首屏 50+ 条流）必然撞到预算线，当场 close 会把正在传输的
    // 响应全部腰斩，浏览器整页失败重试；排空后这些流正常完成，客户端在下一条
    // 新流时才用上重连好的新连接（客户端有多条连接轮转，见 selector 的 muxTarget）。
    this.charge("connect");
    if (this.connectCount >= CONNECT_BUDGET) {
      this.log(`subrequest budget ${this.connectCount} reached, recycling session`);
      console.error(`[exit] budget recycled after ${this.connectCount} subrequests`);
      if (this.streams.size > 0) {
        this.draining = true;
        this.log(`draining ${this.streams.size} in-flight stream(s) before recycle`);
      } else {
        this.closeSessionForBudget();
      }
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
      relayId: ex.relay ? `${ex.relay.host}:${ex.relay.port}` : null,
      prof: p, // 首字节到达时打点用（见 pumpOutbound）
      openedAt: Date.now(),
      // 数据路径的度量（2026-10-04 加）。
      //
      // 为什么必须补这三个：坏窗口里用户等的"几十秒"发生在**建连之后** ——
      // exit.* 那批 span 全都好看（几十毫秒），而真正慢的是"目标多久才回第一个
      // 字节"与"这一路到底有多少吞吐"。此前只有一个 mark（mark 在 summarize 里
      // 只剩名字，看不到耗时分布），所以"中继慢在哪"无从回答。
      //
      // TTFB 的 attrs 带上出口标识：这样慢的那几条能直接对到具体中继，而不是只能
      // 说"中继整体慢"。
      bytes: 0,
      endTTFB: p.begin("exit.ttfb", ex.relay ? `${ex.relay.host}:${ex.relay.port}` : "direct"),
      endStream: p.begin("exit.stream"),
    };
    if (rec.hash) {
      // 会话级出口亲和的证伪点：3 秒内没有首字节，就承认这次选的出口是错的，
      // 把它从缓存里摘掉 —— 下一个流换一条，而不是踩同一脚。
      rec.firstByteTimer = setTimeout(() => {
        rec.firstByteTimer = null;
        this.log(`stream ${id} no first byte in ${FIRST_BYTE_GRACE_MS}ms, forgetting route`);
        this.egress.delete(rec.hash);
      }, FIRST_BYTE_GRACE_MS);
    }
    this.streams.set(id, rec);
    // 计数触发落盘（理由见 PROF_FLUSH_EVERY_STREAMS）：不 await —— 观测不该挡转发。
    this.profStreams += 1;
    if (this.profStreams % PROF_FLUSH_EVERY_STREAMS === 0) {
      this.flushProfile();
    }
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
          // 出站建连成功到目标真的回字节，这一段是"中继/直连选得对不对"的唯一
          // 端到端证据（TCP 通了不代表隧道通）。startup 慢在这里的话，
          // exit.* 的 span 全都好看，只有这个 span 会大。
          rec.prof?.count("firstByte", 1);
          rec.prof?.mark("firstByte", `${Date.now() - rec.openedAt}ms after open`);
          // 首字节到达 ⇒ TTFB 这一段结束。它比 mark 有用得多：mark 在 summarize 里
          // 只剩名字，而 span 有 ms/n/max，能直接看分布。
          if (rec.endTTFB) {
            rec.endTTFB();
            rec.endTTFB = null;
          }
        }
        // 这一路实际送回来的字节数：与 exit.stream 的时长一起给出吞吐
        //（bytes / ms），那是"中继能不能扛住"的直接答案。
        rec.bytes += value.length;
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
      // 数据路径的收尾。
      //
      // 首字节**始终没来**的流要单独记一笔：那正是"隧道建立后被零字节断开"的形态
      //（坏窗口里成片出现，客户端日志里表现为 proxy tunnel dead），而它此前在服务端
      // 完全不可见 —— 那些流的 exit.* span 全是好看的建连耗时。
      if (rec.endTTFB) {
        rec.endTTFB();
        rec.endTTFB = null;
        rec.prof?.count("exit.ttfb.none", 1);
      }
      if (rec.endStream) {
        rec.endStream();
        rec.endStream = null;
      }
      if (rec.bytes > 0) rec.prof?.count("exit.bytes", rec.bytes);
      try { rec.socket.close(); } catch {}
    }
    if (notify && rec?.ws) {
      try { rec.ws.send(encodeCloseControl(id)); } catch {}
    }
    // 排空完成：预算已见底且最后一条在途流结束，现在回收会话。
    if (this.draining && this.streams.size === 0) {
      this.log('drained, recycling session for a fresh connect budget');
      this.closeSessionForBudget();
    }
  }

  rememberClosed(id) {
    if (this.closedIds.size > 4096) this.closedIds.clear();
    this.closedIds.add(id);
  }

  // closeSessionForBudget 回收会话：预算见底后的正常关闭点。
  closeSessionForBudget() {
    this.closeAll();
    try { this.ws?.close(1000, "budget"); } catch {}
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
    // profile 落盘。fire-and-forget：closeAll 在关连接的关键路径上，不能 await；
    // promise 存下来给 webSocketClose / webSocketError 等。
    this.profFlush = this.flushProfile().catch(() => {});
  }
}
