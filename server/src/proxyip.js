// ProxyIP 出口（PRD §7.4，本版只有 HTTP CONNECT 这一种）。
//
// Workers connect() 拨 Cloudflare 自有网段会被平台拒绝（M0 实测），所以目标是 CF
// 承载的站点时只能借第三方中继：connect() 拨中继 → HTTP CONNECT → 隧道里跑的是
// 客户端到目标的原始字节。目标是 HTTPS 时 TLS 由客户端端到端完成，中继只搬运
// 密文——CONNECT 隧道正是为此设计，我们不碰明文。
//
// 出口类型名：写进 Router DO 的 egress_type，KV 条目预留同一字段（PRD §7.4）。


export const RELAY_TYPE_HTTP_CONNECT = "http-connect";

// 中继语义就是反代 CF 的 443，因此只收 443 的条目（与 v0.1 的解析一致）。
export const RELAY_PORT = 443;

// 等 CONNECT 响应头的上限。竞速的 1.5s 单槽超时由 race.js 传进来，这里的值只兜
// 单独调用（不经竞速）的场合。
export const RELAY_CONNECT_TIMEOUT_MS = 5000;

// 中继可用性缓存 TTL。硬编兜底列表必然随时间失效，10 分钟的探活记忆足以把刚
// 失败过的中继踢到候选尾部，而不至于长期锚定一个刚变坏的。
export const RELAY_HEALTH_TTL_MS = 600000;

// 内置兜底列表（CMLiussss 域名型，PRD §8.2）。正式列表由 Cron 拉社区源（ymyuuu/IPDB
// bestproxy 等）写进 KV，兜底列表只管"源不可达时仍有得试"。
export const FALLBACK_RELAY_HOSTS = [
  "ProxyIP.HK.CMLiussss.net",
  "ProxyIP.JP.CMLiussss.net",
  "ProxyIP.KR.CMLiussss.net",
  "ProxyIP.DE.tp2024.CMLiussss.net",
  "ProxyIP.Aliyun.CMLiussss.net",
  "ProxyIP.Oracle.CMLiussss.net",
  "ProxyIP.DigitalOcean.CMLiussss.net",
  "ProxyIP.Vultr.CMLiussss.net",
  "ProxyIP.Multacom.CMLiussss.net",
];

// fallbackRelays 返回兜底候选（复制，调用方可自由打乱）。
export function fallbackRelays() {
  return FALLBACK_RELAY_HOSTS.map((host) => ({ host, port: RELAY_PORT }));
}

// parseRelay 拆 "host[:port]"，端口缺省 443（订阅源格式，纯文本 ip:port）。
export function parseRelay(s) {
  const t = String(s || "").trim();
  const i = t.lastIndexOf(":");
  if (i > 0 && /^\d{1,5}$/.test(t.slice(i + 1))) {
    const port = Number(t.slice(i + 1));
    if (port > 0 && port < 65536) return { host: t.slice(0, i), port };
  }
  return t ? { host: t, port: RELAY_PORT } : null;
}

// ---- 中继健康记忆（内存） ----

const health = new Map(); // "host:port" -> { ok, ms, at }

function relayKey(host, port) {
  return `${host}:${port}`;
}

export function noteRelay(host, port, ok, ms, now) {
  health.set(relayKey(host, port), { ok: !!ok, ms: Number(ms) || 0, at: now ?? Date.now() });
  if (health.size > 256) {
    // Map 保插入序，丢最旧的；健康记录是优化，不是状态。
    health.delete(health.keys().next().value);
  }
}

export function relayHealth(host, port, now) {
  const e = health.get(relayKey(host, port));
  const t = now ?? Date.now();
  if (!e || t - e.at >= RELAY_HEALTH_TTL_MS) {
    health.delete(relayKey(host, port));
    return null;
  }
  return e;
}

// orderByHealth 把 TTL 内探活失败过的中继挪到尾部，其余保持原顺序。
// KV top4 的名次是 Cron 按 EWMA 算出来的，不能被这里打乱；我们只做一件事——
// 别把刚被拒的候选塞进 120ms 内就要启动的前几槽。now 供测试注入时钟。
export function orderByHealth(list, now) {
  const rank = (c) => {
    const h = relayHealth(c.host, c.port, now);
    return !h ? 1 : h.ok ? 0 : 2;
  };
  return list
    .map((c, i) => [rank(c), i, c])
    .sort((a, b) => a[0] - b[0] || a[1] - b[1])
    .map((x) => x[2]);
}

// ---- HTTP CONNECT ----

const CRLFCRLF = "\r\n\r\n";

// withTimeout 给 promise 加时限并且不留下悬挂定时器。
function withTimeout(p, ms, msg) {
  let t = null;
  const timer = new Promise((_, rej) => {
    t = setTimeout(() => rej(new Error(msg)), Math.max(1, ms));
  });
  return Promise.race([p, timer]).finally(() => clearTimeout(t));
}

// readConnectResponse 读到 \r\n\r\n 为止，返回 { status, leftover }。
// leftover 是响应头之后多读到的字节——中继可能在同一段 TCP 里就把目标的首包送来，
// 丢了就是静默吞掉客户端的第一个请求。
async function readConnectResponse(reader, ms) {
  const decoder = new TextDecoder("latin1");
  let buf = new Uint8Array(0);
  for (;;) {
    const chunk = await withTimeout(reader.read(), ms, "relay connect response timeout");
    if (chunk.done) throw new Error("relay closed before CONNECT response");
    const value = chunk.value;
    const next = new Uint8Array(buf.length + value.length);
    next.set(buf, 0);
    next.set(value, buf.length);
    buf = next;
    const i = decoder.decode(buf).indexOf(CRLFCRLF);
    if (i >= 0) {
      const status = Number(decoder.decode(buf.slice(0, i)).split(" ")[1]);
      return { status: Number.isFinite(status) ? status : 0, leftover: buf.slice(i + 4) };
    }
    if (buf.length > 8192) throw new Error("relay CONNECT response too large");
  }
}

// tunnel 把 socket.readable 包一层：先吐 leftover，再继续读。
// 不能直接返回 socket.readable——它的 reader 已被响应头读走，锁不会自动交还。
function tunnel(reader, leftover) {
  let pending = leftover && leftover.length ? leftover : null;
  return new ReadableStream({
    async pull(ctrl) {
      if (pending) {
        ctrl.enqueue(pending);
        pending = null;
        return;
      }
      const { done, value } = await reader.read();
      if (done) ctrl.close();
      else ctrl.enqueue(value);
    },
    cancel(reason) {
      pending = null;
      return reader.cancel(reason);
    },
  });
}

// connectViaProxyIP 拨中继并发 CONNECT，成功后返回可直接 getWriter/getReader 的
// 隧道 socket；失败返回 { error }。opts.timeoutMs 由竞速传入（单槽预算）。
export async function connectViaProxyIP(relayHost, relayPort, targetHost, targetPort, opts = {}) {
  const timeoutMs = Number(opts.timeoutMs) || RELAY_CONNECT_TIMEOUT_MS;
  const t0 = Date.now();
  let socket = null;
  try {
    socket = connect({ hostname: relayHost, port: relayPort });
    // 超时后我们先走人，opened 这时再拒绝就成了没人接的 promise 拒绝（平台会打日志）。
    socket.opened.catch(() => {});
    await withTimeout(socket.opened, timeoutMs, "relay connect timeout");
    const writer = socket.writable.getWriter();
    try {
      await writer.write(
        new TextEncoder().encode(
          `CONNECT ${targetHost}:${targetPort} HTTP/1.1\r\nHost: ${targetHost}:${targetPort}${CRLFCRLF}`
        )
      );
    } finally {
      // 交还写锁：隧道 socket 的 writable 要交给上层（每流一个 writer）。
      writer.releaseLock();
    }
    const reader = socket.readable.getReader();
    const { status, leftover } = await readConnectResponse(
      reader,
      timeoutMs - (Date.now() - t0)
    );
    if (status < 200 || status > 299) throw new Error(`relay CONNECT status ${status}`);
    noteRelay(relayHost, relayPort, true, Date.now() - t0);
    return {
      socket: {
        readable: tunnel(reader, leftover),
        writable: socket.writable,
        close: () => {
          try { socket.close(); } catch {}
        },
      },
    };
  } catch (e) {
    try { socket?.close(); } catch {}
    noteRelay(relayHost, relayPort, false, Date.now() - t0);
    return { error: `${e.message || e} (${Date.now() - t0}ms)` };
  }
}