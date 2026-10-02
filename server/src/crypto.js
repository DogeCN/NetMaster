// 鉴权原语（协议 v2，见 PRD §5）：
//   AUTH = HMAC-SHA256(PASSWORD, TS ‖ STREAM_ID ‖ ATYP ‖ ADDR ‖ PORT)[:16]
// 签名覆盖首帧 AUTH 之后的全部字节，因此校验端直接对 frame.slice(16) 重算即可。
// TS ±300s 窗口只做廉价一致性校验；重放防护有意从轻（连接在 TLS 之内）。

export const AUTH_LEN = 16;
export const TS_WINDOW_SEC = 300;

export function utf8(s) {
  return new TextEncoder().encode(s);
}

// authCode 对签名区（TS‖ID‖ATYP‖ADDR‖PORT）计算截断 HMAC。
export async function authCode(password, signed) {
  const key = await crypto.subtle.importKey(
    "raw",
    utf8(password),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"]
  );
  const sig = await crypto.subtle.sign("HMAC", key, signed);
  return new Uint8Array(sig).slice(0, AUTH_LEN);
}

// tsFromBytes 读 8 字节大端 Unix 秒。
export function tsFromBytes(b) {
  let v = 0n;
  for (let i = 0; i < 8; i++) v = (v << 8n) | BigInt(b[i]);
  return v;
}

export function tsWithinWindow(ts, nowSec, windowSec = TS_WINDOW_SEC) {
  const now = typeof nowSec === "number" ? BigInt(Math.floor(nowSec)) : nowSec;
  const d = ts > now ? ts - now : now - ts;
  return d <= BigInt(windowSec);
}

// 常量时间比较；crypto.subtle.timingSafeEqual 是 Workers 专属 API，
// Node（单测环境）没有，退回逐字节异或累加。
export function safeEqualBytes(a, b) {
  if (a.length !== b.length) return false;
  if (typeof crypto.subtle.timingSafeEqual === "function") {
    return crypto.subtle.timingSafeEqual(a, b);
  }
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}
