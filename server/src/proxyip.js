// ProxyIP 出口（PRD §7.4）。两种中继语义：
//
//   sni（公共 CMLiussss 中继全是这一类）：不是 HTTP CONNECT 代理 —— 实测对
//   CONNECT 请求回 400。它读客户端 TLS ClientHello 里的 SNI 并据此转发
//   （v0.1.x 的 resolveOverride 走的就是同一种机制）。worker 只需把隧道建起来，
//   字节原样双向灌，ClientHello 会带着真实 SNI 经过它。
//
//   http-connect（自建 VPS）：connect() 拨中继 → HTTP CONNECT → 隧道。目标是
//   HTTPS 时 TLS 由客户端端到端完成，中继只搬运密文。
//
// 出口类型名：写进 Router DO 的 egress_type，KV 条目带同一字段（PRD §7.4）。

import { connect } from './socket.js';
import { resolve4 } from './exits.js';

// resolver 可注入：测试里把 DoH 换成恒等解析，避免单测打真实网络。
let resolve4Impl = resolve4;
export function setResolver(fn) {
  resolve4Impl = fn;
}

export const RELAY_TYPE_HTTP_CONNECT = "http-connect";
export const RELAY_TYPE_SNI = "sni";

// 中继语义就是反代 CF 的 443，因此只收 443 的条目（与 v0.1 的解析一致）。
export const RELAY_PORT = 443;

// 等 CONNECT 响应头的上限。单独调用（不经 order.js 顺序尝试）的场合兜底用。
export const RELAY_CONNECT_TIMEOUT_MS = 5000;

// 候选列表不在这里：硬编在部署工作流（deploy.yml 的 RELAYS），部署时测速排序
// 写进 KV 的 proxyip:top，Worker 侧由 order.js 顺序消费并就地重排。
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

// connectViaSNI 拨 SNI 路由型中继（CMLiussss 公共中继全是这一类）。
//
// 这类中继不是 HTTP CONNECT 代理 —— 实测对 CONNECT 请求回 400。它读的是
// 客户端 TLS ClientHello 里的 SNI 并据此转发（v0.1.x 用 resolveOverride 走的
// 就是同一种机制）。所以这里只需要把隧道建起来，字节原样双向灌：
// 客户端的 ClientHello 会带着真实 SNI 经过它。
export async function connectViaSNI(relayHost, relayPort, opts = {}) {
  const timeoutMs = opts.timeoutMs || RELAY_CONNECT_TIMEOUT_MS;
  let socket;
  try {
    let relayIp = relayHost;
    if (!/^(\d{1,3}\.){3}\d{1,3}$/.test(relayHost)) {
      const ips = await resolve4Impl(relayHost);
      if (!ips.length) return { error: `dns: no A record for relay ${relayHost}` };
      relayIp = ips[0];
    }
    socket = connect({ hostname: relayIp, port: relayPort });
    await withTimeout(socket.opened, timeoutMs, "relay connect timeout");
    return { socket };
  } catch (e) {
    try { socket?.close(); } catch {}
    return { error: e.message || e };
  }
}

// dialRelay 按 KV 条目的 type 分发。缺省 sni：公共源（CMLiussss 域名、IPDB 的
// ProxyIP 条目）全是 SNI 路由型，HTTP CONNECT 只在自建 VPS 中继上出现。
export async function dialRelay(relay, targetHost, targetPort, opts = {}) {
  if (relay.type === RELAY_TYPE_HTTP_CONNECT) {
    return connectViaProxyIP(relay.host, relay.port, targetHost, targetPort, opts);
  }
  return connectViaSNI(relay.host, relay.port, opts);
}

// connectViaProxyIP 拨中继并发 CONNECT，成功后返回可直接 getWriter/getReader 的
// 隧道 socket；失败返回 { error }。opts.timeoutMs 由竞速传入（单槽预算）。
export async function connectViaProxyIP(relayHost, relayPort, targetHost, targetPort, opts = {}) {
  const timeoutMs = Number(opts.timeoutMs) || RELAY_CONNECT_TIMEOUT_MS;
  const t0 = Date.now();
  let socket = null;
  try {
    // 中继可能是域名（CMLiussss 全系）：connect() 只吃 IP 字面量，先解析。
    let relayIp = relayHost;
    if (!/^([0-9]{1,3}\.){3}[0-9]{1,3}$/.test(relayHost)) {
      const ips = await resolve4Impl(relayHost);
      if (!ips.length) return { error: `dns: no A record for relay ${relayHost}` };
      relayIp = ips[0];
    }
    socket = connect({ hostname: relayIp, port: relayPort });
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
    return { error: `${e.message || e} (${Date.now() - t0}ms)` };
  }
}