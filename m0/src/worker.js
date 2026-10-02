// M0 平台核验探针：验证 NetMaster v2 设计所依赖的 Cloudflare Workers/DO 平台假设。
// 临时部署，测量完成后整个 m0/ 删除。所有端点要求 ?t=<PROBE_TOKEN>，否则 404 空 body。
//
// 实验（编号对应 docs/m0-findings.md）：
//   E1  WS 消息计数跨 Hibernation 持久（对照 dashboard 用量验证 20:1 折算）
//   E2  单 DO 内 >6 条并发出站 connect()；另设 worker 侧对照
//   E3  connect() IPv6 字面量 + NAT64 合成地址连通性
//   E4  开着出站 TCP socket 的 DO 休眠/唤醒后 socket 是否存活
//
// E4 的休眠判定：openSocket 时设 20s Alarm。Alarm 若在全新实例中触发
// （instanceSockets 为空而 globalThis 标记仍在），说明此前发生过休眠；
// 之后再 check/write 全局表中的 socket，即为"跨休眠存活"的直接证据。

import { DurableObject } from "cloudflare:workers";
import { connect } from "cloudflare:sockets";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function concat(chunks) {
  const total = chunks.reduce((a, c) => a + c.byteLength, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.byteLength;
  }
  return out;
}

// TCP 连通性探测：connect -> (可选 write) -> (可选 read 一次)。
async function probeConnect(url) {
  const host = url.searchParams.get("host");
  const port = parseInt(url.searchParams.get("port") || "443");
  const payload = url.searchParams.get("payload");
  const waitMs = Math.min(parseInt(url.searchParams.get("waitMs") || "4000"), 15000);
  if (!host || !port) return Response.json({ ok: false, error: "host/port required" }, { status: 400 });

  const t0 = Date.now();
  let socket;
  try {
    socket = connect({ hostname: host, port });
    await Promise.race([
      socket.opened,
      sleep(waitMs).then(() => {
        throw new Error("connect timeout");
      }),
    ]);
  } catch (e) {
    try {
      socket?.close();
    } catch {}
    return Response.json({ ok: false, phase: "connect", error: String(e), ms: Date.now() - t0 });
  }
  const ms = Date.now() - t0;

  let readResult = null;
  if (payload) {
    try {
      const w = socket.writable.getWriter();
      await w.write(new TextEncoder().encode(payload));
      w.releaseLock();
      const r = socket.readable.getReader();
      const chunk = await Promise.race([
        r.read(),
        sleep(2500).then(() => {
          throw new Error("read timeout");
        }),
      ]);
      readResult = chunk.done ? "eof" : `${chunk.value.byteLength} bytes: ${new TextDecoder().decode(chunk.value).slice(0, 200)}`;
      r.releaseLock();
    } catch (e) {
      readResult = "error: " + String(e);
    }
  }
  try {
    socket.close();
  } catch {}
  return Response.json({ ok: true, host, port, ms, readResult });
}

// 并发出站：n 条 connect() 同时发起，统计成功数（E2）。
async function runConcurrent(url) {
  const n = Math.min(Math.max(parseInt(url.searchParams.get("n") || "8"), 1), 32);
  const host = url.searchParams.get("host") || "8.8.8.8";
  const port = parseInt(url.searchParams.get("port") || "443");
  const jobs = Array.from({ length: n }, (_, i) =>
    probeConnect(new URL(`https://x/?host=${encodeURIComponent(host)}&port=${port}&waitMs=8000`)).then((r) => r.json())
  );
  const results = await Promise.all(jobs);
  return Response.json({
    n,
    host,
    port,
    succeeded: results.filter((r) => r.ok).length,
    results,
  });
}

async function runDoh(url) {
  const name = url.searchParams.get("name") || "example.com";
  const t0 = Date.now();
  try {
    const r = await fetch(`https://cloudflare-dns.com/dns-query?name=${encodeURIComponent(name)}&type=A`, {
      headers: { accept: "application/dns-json" },
    });
    const j = await r.json();
    return Response.json({ ok: true, status: r.status, ms: Date.now() - t0, answers: j.Answer || [] });
  } catch (e) {
    return Response.json({ ok: false, error: String(e), ms: Date.now() - t0 });
  }
}

export default {
  async fetch(req, env) {
    const url = new URL(req.url);
    if (url.searchParams.get("t") !== env.PROBE_TOKEN) return new Response(null, { status: 404 });

    if (url.pathname === "/ws") {
      if (req.headers.get("Upgrade") !== "websocket") return new Response(null, { status: 404 });
      const id = env.PROBE.idFromName(url.searchParams.get("do") || "default");
      return env.PROBE.get(id).fetch(req);
    }
    if (url.pathname === "/w/concurrent") return runConcurrent(url);
    if (url.pathname === "/w/connect") return probeConnect(url);
    if (url.pathname === "/w/doh") return runDoh(url);

    if (url.pathname === "/w/budget") {
      // 顺序消耗：单次调用里连续 fetch(DoH)+connect 对，数到第几个被
      // "Too many subrequests" 拒绝。n 默认 30。
      const n = Math.min(parseInt(url.searchParams.get("n") || "30"), 60);
      const events = [];
      let socket;
      for (let i = 1; i <= n; i++) {
        try {
          const r = await fetch("https://cloudflare-dns.com/dns-query?name=example.com&type=A", {
            headers: { accept: "application/dns-json" },
          });
          events.push(`${i}:fetch ${r.status}`);
          socket = connect({ hostname: "8.8.8.8", port: 443 });
          await socket.opened;
          events.push(`${i}:connect ok`);
          socket.close();
        } catch (e) {
          events.push(`${i}:FAIL ${String(e.message || e).slice(0, 80)}`);
          try { socket?.close(); } catch {}
          break;
        }
      }
      return Response.json({ events, completed: events.filter((e) => e.includes("ok")).length / 2 });
    }

    if (url.pathname.startsWith("/do/")) {
      const name = url.pathname.split("/")[2];
      if (!name) return new Response(null, { status: 404 });
      const id = env.PROBE.idFromName(name);
      return env.PROBE.get(id).fetch(req);
    }
    return new Response(null, { status: 404 });
  },
};

export class ProbeDO extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.sockets = new Map(); // 实例内存：tag -> socket 记录（休眠即失）
    if (!globalThis.__m0) globalThis.__m0 = { born: new Date().toISOString() };
    this.ctx.storage.sql.exec("CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT)");
  }

  kvGet(k) {
    const rows = [...this.ctx.storage.sql.exec("SELECT v FROM kv WHERE k = ?", k)];
    return rows.length ? rows[0].v : null;
  }

  kvSet(k, v) {
    this.ctx.storage.sql.exec("INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v", k, v);
  }

  bump() {
    this.kvSet("msgCount", Number(this.kvGet("msgCount") || 0) + 1);
  }

  async fetch(req) {
    const url = new URL(req.url);
    let p = url.pathname;
    const m = p.match(/^\/do\/[^/]+(\/.*)?$/);
    if (m) p = m[1] || "/";
    if (req.headers.get("Upgrade") === "websocket") {
      const pair = new WebSocketPair();
      this.ctx.acceptWebSocket(pair[1]); // Hibernation API
      this.bump();
      return new Response(null, { status: 101, webSocket: pair[0] });
    }
    switch (p) {
      case "/count":
        return Response.json({
          msgCount: Number(this.kvGet("msgCount") || 0),
          alarmEvents: JSON.parse(this.kvGet("alarmEvents") || "[]"),
        });
      case "/concurrent":
        return runConcurrent(url);
      case "/connect":
        return probeConnect(url);
      default:
        return new Response(null, { status: 404 });
    }
  }

  async webSocketMessage(ws, message) {
    this.bump();
    const cmd = String(message);
    let reply;
    try {
      if (cmd.startsWith("open:")) {
        const [, tag, host, port] = cmd.split(":");
        reply = await this.openSocket(tag, host, parseInt(port));
      } else if (cmd.startsWith("write:")) {
        const i1 = cmd.indexOf(":");
        const i2 = cmd.indexOf(":", i1 + 1);
        reply = await this.writeSocket(tagOf(cmd), atob(cmd.slice(i2 + 1)));
      } else if (cmd.startsWith("check:")) {
        reply = await this.checkSocket(tagOf(cmd));
      } else if (cmd === "meta") {
        reply = this.meta();
      } else {
        reply = { echo: cmd, note: "counted" };
      }
    } catch (e) {
      reply = { error: String(e) };
    }
    ws.send(JSON.stringify(reply));
  }

  async webSocketClose(ws) {
    this.bump();
  }

  async openSocket(tag, host, port) {
    if (this.sockets.has(tag) || globalThis.__m0sockets?.has(tag)) return { tag, note: "already open" };
    const socket = connect({ hostname: host, port });
    await Promise.race([
      socket.opened,
      sleep(4000).then(() => {
        throw new Error("connect timeout");
      }),
    ]);
    const rec = { host, port, socket, chunks: [], done: false, error: null };
    // 后台读循环：持续收 chunks，供 write 后检查是否有回包。
    (async () => {
      try {
        const r = socket.readable.getReader();
        for (;;) {
          const { done, value } = await r.read();
          if (done) break;
          rec.chunks.push(value);
        }
        rec.done = true;
      } catch (e) {
        rec.error = String(e);
        rec.done = true;
      }
    })();
    this.sockets.set(tag, rec);
    globalThis.__m0sockets = globalThis.__m0sockets || new Map();
    globalThis.__m0sockets.set(tag, rec);
    // 20s 后 Alarm：若在全新实例触发（instanceSockets 空），即证明中间发生过休眠。
    this.ctx.storage.setAlarm(Date.now() + 20000);
    return { tag, opened: true, host, port };
  }

  async writeSocket(tag, payload) {
    const rec = this.find(tag);
    if (!rec) return { tag, where: "none" };
    try {
      const w = rec.socket.writable.getWriter();
      await w.write(new TextEncoder().encode(payload));
      w.releaseLock();
      await sleep(1500);
      const bytes = rec.chunks.reduce((a, c) => a + c.byteLength, 0);
      const sample = bytes ? new TextDecoder().decode(concat(rec.chunks)).slice(0, 300) : "";
      return { tag, where: this.sockets.has(tag) ? "instance" : "global", wrote: true, readBytes: bytes, sample };
    } catch (e) {
      return { tag, where: this.sockets.has(tag) ? "instance" : "global", wrote: false, error: String(e) };
    }
  }

  async checkSocket(tag) {
    const rec = this.find(tag);
    if (!rec) return { tag, where: "none" };
    let closed;
    try {
      closed = await Promise.race([
        rec.socket.closed.then(() => "closed", () => "errored"),
        Promise.resolve("open"),
      ]);
    } catch (e) {
      closed = "race-error: " + String(e);
    }
    return {
      tag,
      where: this.sockets.has(tag) ? "instance" : "global",
      closed,
      readerDone: rec.done,
      readBytes: rec.chunks.reduce((a, c) => a + c.byteLength, 0),
      readerError: rec.error,
    };
  }

  find(tag) {
    if (this.sockets.has(tag)) return this.sockets.get(tag);
    if (globalThis.__m0sockets?.has(tag)) return globalThis.__m0sockets.get(tag);
    return null;
  }

  meta() {
    return {
      isolateBorn: globalThis.__m0.born,
      instanceSockets: [...this.sockets.keys()],
      globalSockets: [...(globalThis.__m0sockets?.keys() || [])],
      msgCount: Number(this.kvGet("msgCount") || 0),
      alarmEvents: JSON.parse(this.kvGet("alarmEvents") || "[]"),
    };
  }

  async alarm() {
    const events = JSON.parse(this.kvGet("alarmEvents") || "[]");
    events.push({
      firedAt: new Date().toISOString(),
      isolateBorn: globalThis.__m0.born,
      instanceSockets: [...this.sockets.keys()],
      globalSockets: [...(globalThis.__m0sockets?.keys() || [])],
    });
    this.kvSet("alarmEvents", JSON.stringify(events));
  }
}

function tagOf(cmd) {
  const i1 = cmd.indexOf(":");
  const i2 = cmd.indexOf(":", i1 + 1);
  return i2 === -1 ? cmd.slice(i1 + 1) : cmd.slice(i1 + 1, i2);
}
