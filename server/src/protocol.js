// 协议 v2 帧编解码（PRD §4）。全部多字节字段大端。
//
//   首帧   AUTH(16) | TS(8) | STREAM_ID(4) | ATYP(1) | ADDR | PORT(2)
//   开帧   STREAM_ID(4) | ATYP(1) | ADDR | PORT(2)
//   数据帧 STREAM_ID(4) | PAYLOAD            PAYLOAD ≤ 64 KB
//   控制帧 0x00000000 | CTRL_TYPE(1) | ...    总长 ≤ 9 字节
//   响应帧 STREAM_ID(4) | STATUS(1)
//
// 流 ID ∈ [1, 0xFFFFFFFE]；0 是控制帧专用前缀，0xFFFFFFFF 永久保留。
// 递增到 0xFFFFFFFE 后回绕到 1。
//
// 开帧与数据帧在裸字节上同形（ID 开头），区分靠流状态机：客户端在收到该流的
// STATUS 0x00 之前不发任何数据帧（PRD §4.4），因此服务端只对“流表里不存在的
// ID”按开帧解析。

import { utf8, tsFromBytes } from './crypto.js';

export const ATYP_IPV4 = 1;
export const ATYP_DOMAIN = 2;
export const ATYP_IPV6 = 3;

export const STATUS_OK = 0;
export const STATUS_BAD = 1; // 格式或认证错误
export const STATUS_FORBIDDEN = 2; // 目标禁连
export const STATUS_NOEXIT = 3; // 出口全部失败

export const CTRL_CLOSE = 1;

export const MAX_PAYLOAD = 64 * 1024;
export const STREAM_ID_MAX = 0xfffffffe;

export function validStreamId(id) {
  return id >= 1 && id <= STREAM_ID_MAX;
}

// ---- 地址编解码 ----

// encodeAddr 把 host/port 编成 ATYP | ADDR | PORT。host 是 IPv4 字面量、
// IPv6 字面量或域名；解析失败返回 null。
export function encodeAddr(host, port) {
  let atyp;
  let addr;
  if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host)) {
    const parts = host.split(".").map(Number);
    if (parts.some((n) => n > 255)) return null;
    atyp = ATYP_IPV4;
    addr = Uint8Array.from(parts);
  } else if (host.includes(":")) {
    const bin = parseIPv6(host);
    if (!bin) return null;
    atyp = ATYP_IPV6;
    addr = bin;
  } else {
    const lower = host.toLowerCase();
    // 与 parseAddr 同一套白名单。服务端的 parseAddr 才是安全边界（那里校验的是
    // 不可信的网络输入），这里只是不让客户端把垃圾送上线路 —— 少一个要解释的
    // "为什么客户端会把 CR/LF 编进目标"。
    if (!/^[a-z0-9._-]+$/.test(lower)) return null;
    const raw = utf8(lower);
    if (raw.length < 1 || raw.length > 255) return null;
    atyp = ATYP_DOMAIN;
    addr = Uint8Array.of(raw.length, ...raw);
  }
  const out = new Uint8Array(1 + addr.length + 2);
  out[0] = atyp;
  out.set(addr, 1);
  out[out.length - 2] = (port >> 8) & 0xff;
  out[out.length - 1] = port & 0xff;
  return out;
}

// parseAddr 解析 ATYP | ADDR | PORT，返回 { atyp, addr, port, len }。
// addr 归一为字符串（域名小写）；失败返回 null。len 是消耗的字节数。
export function parseAddr(b, off) {
  if (off >= b.length) return null;
  const atyp = b[off];
  let addr;
  let len;
  switch (atyp) {
    case ATYP_IPV4:
      if (off + 1 + 4 + 2 > b.length) return null;
      addr = `${b[off + 1]}.${b[off + 2]}.${b[off + 3]}.${b[off + 4]}`;
      len = 7;
      break;
    case ATYP_DOMAIN: {
      if (off + 2 > b.length) return null;
      const n = b[off + 1];
      if (n < 1 || off + 2 + n + 2 > b.length) return null;
      const raw = b.slice(off + 2, off + 2 + n);
      // 主机名必须是**白名单字符**，不是"没坏字节就行"。
      //
      // 这个字符串下游会被 proxyip.js 逐字插进发往第三方 http-connect 中继的**裸
      // HTTP 请求**。TextDecoder 是非致命的：CR/LF 原样通过，于是
      // "a.example\r\nx-injected: pwned\r\n\r\nGET /evil:443 HTTP/1.1" 就能把那条
      // 请求劈成两条 —— 对池里任何 http-connect 型中继都是实打实的请求拆分。
      //
      // 真正的域名只会用到这些字符（IDN 进来时已是 punycode，同样是 ASCII），
      // 所以白名单不会误伤正常目标；而控制字符、空格、CR/LF 一并出局。
      for (const byte of raw) {
        const ok =
          (byte >= 0x61 && byte <= 0x7a) || // a-z
          (byte >= 0x30 && byte <= 0x39) || // 0-9
          byte === 0x2d || // -
          byte === 0x2e || // .
          byte === 0x5f; // _ （部分内网服务确实用）
        if (!ok) return null;
      }
      addr = new TextDecoder().decode(raw).toLowerCase();
      len = 4 + n;
      break;
    }
    case ATYP_IPV6:
      if (off + 1 + 16 + 2 > b.length) return null;
      addr = formatIPv6(b.slice(off + 1, off + 17));
      len = 17;
      break;
    default:
      return null;
  }
  const port = (b[off + len - 2] << 8) | b[off + len - 1];
  return { atyp, addr, port, len };
}

// ---- 帧编解码 ----

// encodeFirstFrame 构造首帧；auth 由调用方计算（签名区 = 除 AUTH 外的全部字节）。
export function encodeFirstFrame(auth, tsSec, streamId, host, port) {
  const addr = encodeAddr(host, port);
  if (!addr) return null;
  const out = new Uint8Array(16 + 8 + 4 + addr.length);
  out.set(auth, 0);
  const ts = BigInt(tsSec);
  for (let i = 0; i < 8; i++) out[16 + i] = Number((ts >> BigInt(56 - 8 * i)) & 0xffn);
  out[24] = (streamId >>> 24) & 0xff;
  out[25] = (streamId >>> 16) & 0xff;
  out[26] = (streamId >>> 8) & 0xff;
  out[27] = streamId & 0xff;
  out.set(addr, 28);
  return out;
}

// parseFirstFrame 校验首帧结构并返回 { ts, streamId, host, port, signed }。
// signed 是 TS 起的全部字节（HMAC 的输入区）。结构性失败返回 null；
// 认证校验在 session.js 里做（需要 password）。
export function parseFirstFrame(b) {
  if (b.length < 16 + 8 + 4 + 2) return null; // 不够到 PORT 就没有解析意义
  const ts = tsFromBytes(b.slice(16, 24));
  const streamId = (b[24] << 24) | (b[25] << 16) | (b[26] << 8) | b[27];
  const a = parseAddr(b, 28);
  if (!a) return null;
  return { ts, streamId, host: a.addr, port: a.port, signed: b.slice(16) };
}

// encodeOpenFrame 构造开帧。
export function encodeOpenFrame(streamId, host, port) {
  const addr = encodeAddr(host, port);
  if (!addr) return null;
  const out = new Uint8Array(4 + addr.length);
  out[0] = (streamId >>> 24) & 0xff;
  out[1] = (streamId >>> 16) & 0xff;
  out[2] = (streamId >>> 8) & 0xff;
  out[3] = streamId & 0xff;
  out.set(addr, 4);
  return out;
}

// parseOpenFrame 解析开帧，返回 { streamId, host, port } 或 null。
export function parseOpenFrame(b) {
  if (b.length < 4) return null;
  const streamId = (b[0] << 24) | (b[1] << 16) | (b[2] << 8) | b[3];
  const a = parseAddr(b, 4);
  if (!a) return null;
  return { streamId, host: a.addr, port: a.port };
}

export function encodeDataFrame(streamId, payload) {
  const out = new Uint8Array(4 + payload.length);
  out[0] = (streamId >>> 24) & 0xff;
  out[1] = (streamId >>> 16) & 0xff;
  out[2] = (streamId >>> 8) & 0xff;
  out[3] = streamId & 0xff;
  out.set(payload, 4);
  return out;
}

export function encodeResponse(streamId, status) {
  return Uint8Array.of(
    (streamId >>> 24) & 0xff,
    (streamId >>> 16) & 0xff,
    (streamId >>> 8) & 0xff,
    streamId & 0xff,
    status
  );
}

export function encodeCloseControl(streamId) {
  return Uint8Array.of(
    0, 0, 0, 0, CTRL_CLOSE,
    (streamId >>> 24) & 0xff,
    (streamId >>> 16) & 0xff,
    (streamId >>> 8) & 0xff,
    streamId & 0xff
  );
}

// isControl 前缀 0x00000000 判定；流 ID ≥ 1 的数据帧首字节不可能是 0。
export function isControl(b) {
  return b.length >= 4 && b[0] === 0 && b[1] === 0 && b[2] === 0 && b[3] === 0;
}

// parseCloseControl 解析 CLOSE 控制帧，返回 streamId 或 null。
export function parseCloseControl(b) {
  if (b.length < 9 || b[4] !== CTRL_CLOSE) return null;
  return (b[5] << 24) | (b[6] << 16) | (b[7] << 8) | b[8];
}

export function streamIdFromBytes(b) {
  return (b[0] << 24) | (b[1] << 16) | (b[2] << 8) | b[3];
}

// ---- IPv6 字面量辅助 ----

// parseIPv6 解析 IPv6 字面量为 16 字节；支持 :: 缩写与内嵌 IPv4。失败返回 null。
export function parseIPv6(s) {
  s = s.replace(/^\[|\]$/g, "");
  const halves = s.split("::");
  if (halves.length > 2) return null;
  const parseGroups = (part) => {
    if (part === "") return [];
    const groups = [];
    const items = part.split(":");
    for (let i = 0; i < items.length; i++) {
      const it = items[i];
      if (it.includes(".")) {
        if (i !== items.length - 1) return null;
        const m = it.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
        if (!m) return null;
        const octs = m.slice(1).map(Number);
        if (octs.some((n) => n > 255)) return null;
        groups.push((octs[0] << 8) | octs[1], (octs[2] << 8) | octs[3]);
      } else {
        if (!/^[0-9a-fA-F]{1,4}$/.test(it)) return null;
        groups.push(parseInt(it, 16));
      }
    }
    return groups;
  };
  let head;
  let tail;
  if (halves.length === 2) {
    head = parseGroups(halves[0]);
    tail = parseGroups(halves[1]);
    if (head === null || tail === null) return null;
    if (head.length + tail.length > 7) return null;
  } else {
    head = parseGroups(halves[0]);
    if (head === null || head.length !== 8) return null;
    tail = [];
  }
  const groups = [...head, ...Array(8 - head.length - tail.length).fill(0), ...tail];
  const out = new Uint8Array(16);
  for (let i = 0; i < 8; i++) {
    out[2 * i] = groups[i] >> 8;
    out[2 * i + 1] = groups[i] & 0xff;
  }
  return out;
}

// formatIPv6 输出展开形式（每组 4 位十六进制、零不压缩）——展开形式是合法的
// IPv6 字面量，connect() 与测试都直接可用。
export function formatIPv6(b) {
  const groups = [];
  for (let i = 0; i < 8; i++) groups.push(((b[2 * i] << 8) | b[2 * i + 1]).toString(16).padStart(4, "0"));
  return groups.join(":");
}
