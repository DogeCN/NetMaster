// 出口层（M1：直连出口）。目标禁连清单（PRD §4.4 STATUS 0x02）：
//   私网/保留网段、Cloudflare 自有网段（connect() 禁拨，提前拒绝省一次失败）、端口 25。
// M3 在这里追加 ProxyIP 出口与竞速。

import { parseIPv6, ATYP_IPV4, ATYP_IPV6, ATYP_DOMAIN } from './protocol.js';


// ---- 网段匹配 ----

// v4 地址 → uint32。
export function ipv4ToUint32(s) {
  const m = s.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (!m) return null;
  const o = m.slice(1).map(Number);
  if (o.some((n) => n > 255)) return null;
  return (((o[0] << 24) | (o[1] << 16) | (o[2] << 8) | o[3]) >>> 0);
}

// v6 地址 → 16 字节数组（parseIPv6 来自 protocol.js）。
export function ipv6ToBytes(s) {
  return parseIPv6(s);
}

export function inCidr4(ip, cidr) {
  const [net, bits] = cidr.split("/");
  const nip = ipv4ToUint32(ip);
  const base = ipv4ToUint32(net);
  if (nip === null || base === null) return false;
  const mask = bits === "0" ? 0 : (0xffffffff << (32 - Number(bits))) >>> 0;
  return (nip & mask) === (base & mask);
}

export function inCidr6(ipBytes, cidr) {
  const [net, bits] = cidr.split("/");
  const base = ipv6ToBytes(net);
  if (!ipBytes || !base) return false;
  const n = Number(bits);
  let remaining = n;
  for (let i = 0; i < 16 && remaining > 0; i++) {
    const take = Math.min(8, remaining);
    const mask = take === 8 ? 0xff : (0xff << (8 - take)) & 0xff;
    if ((ipBytes[i] & mask) !== (base[i] & mask)) return false;
    remaining -= take;
  }
  return true;
}

// ---- 禁连清单 ----

// RFC 1918/5735/3849 + 组播/保留（224/4 覆盖 240/4 与 255.255.255.255）。
export const PRIVATE_V4 = [
  "0.0.0.0/8",
  "10.0.0.0/8",
  "100.64.0.0/10",
  "127.0.0.0/8",
  "169.254.0.0/16",
  "172.16.0.0/12",
  "192.0.0.0/24",
  "192.0.2.0/24",
  "192.168.0.0/16",
  "198.18.0.0/15",
  "198.51.100.0/24",
  "203.0.113.0/24",
  "224.0.0.0/3",
];

// cloudflare.com/ips 公布的全部 IPv4 网段。
export const CF_V4 = [
  "173.245.48.0/20",
  "103.21.244.0/22",
  "103.22.200.0/22",
  "103.31.4.0/22",
  "141.101.64.0/18",
  "108.162.192.0/18",
  "190.93.240.0/20",
  "188.114.96.0/20",
  "197.234.240.0/22",
  "198.41.128.0/17",
  "162.158.0.0/15",
  "104.16.0.0/13",
  "104.24.0.0/14",
  "172.64.0.0/13",
  "131.0.72.0/22",
];

export const PRIVATE_V6 = ["::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "2001:db8::/32"];
export const CF_V6 = [
  "2400:cb00::/32",
  "2606:4700::/32",
  "2803:f800::/32",
  "2405:b500::/32",
  "2405:8100::/32",
  "2a06:98c0::/29",
  "2c0f:f248::/32",
];

// isForbidden 判定目标是否禁连。atyp/addr 来自 parseAddr（addr 已是字符串）。
// 返回 true=禁连。域名不做预检（解析后的限制由运行时拒绝，表现为出口失败）。
export function isForbidden(atyp, addr, port) {
  if (port === 25) return true;
  if (atyp === ATYP_IPV4) {
    for (const c of PRIVATE_V4) if (inCidr4(addr, c)) return true;
    for (const c of CF_V4) if (inCidr4(addr, c)) return true;
    return false;
  }
  if (atyp === ATYP_IPV6) {
    const b = ipv6ToBytes(addr);
    if (!b) return false; // 解析不了交给运行时
    // IPv4-mapped (::ffff:a.b.c.d) 按其内嵌 IPv4 判定。
    const mapped = b[10] === 0xff && b[11] === 0xff && b.slice(0, 10).every((x) => x === 0);
    if (mapped) {
      const v4 = `${b[12]}.${b[13]}.${b[14]}.${b[15]}`;
      for (const c of PRIVATE_V4) if (inCidr4(v4, c)) return true;
      for (const c of CF_V4) if (inCidr4(v4, c)) return true;
      return false;
    }
    for (const c of PRIVATE_V6) if (inCidr6(b, c)) return true;
    for (const c of CF_V6) if (inCidr6(b, c)) return true;
    return false;
  }
  return false;
}

// ---- DNS 与直连出口 ----

export const CONNECT_TIMEOUT_MS = 15000;
export const DOH_TIMEOUT_MS = 4000;

// 平台规则（M0 探针 E6/E7 实测，docs/m0-findings.md）：
//   connect() 只接受 IP 字面量 —— 域名目标被平台直接拒绝（"consider using fetch"）；
//   80 端口禁拨；CF 网段禁拨；无 IPv6。所以域名目标必须先自己解析成 A 记录。
// 解析走 DoH（fetch 出站通道，不受 connect() 禁连清单约束），结果进程内缓存。

const dnsCache = new Map(); // host -> { ips, at }
const DNS_TTL_MS = 300000;

export async function resolve4(host) {
  if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host)) return [host]; // 已是 IPv4 字面量
  const cached = dnsCache.get(host);
  if (cached && Date.now() - cached.at < DNS_TTL_MS) return cached.ips;

  const endpoints = [
    "https://cloudflare-dns.com/dns-query?name=" + encodeURIComponent(host) + "&type=A",
    "https://dns.google/resolve?name=" + encodeURIComponent(host) + "&type=A",
  ];
  for (const url of endpoints) {
    try {
      const r = await fetch(url, {
        headers: { accept: "application/dns-json" },
        signal: AbortSignal.timeout(DOH_TIMEOUT_MS),
      });
      if (!r.ok) continue;
      const j = await r.json();
      const ips = (j.Answer || []).filter((a) => a.type === 1).map((a) => a.data);
      if (ips.length) {
        dnsCache.set(host, { ips, at: Date.now() });
        return ips;
      }
      // NXDOMAIN 也不必再试下一个端点：记一个空结果快速失败
      dnsCache.set(host, { ips: [], at: Date.now() });
      return [];
    } catch {
      // 换下一个 DoH 端点
    }
  }
  return [];
}

// directConnect 用 connect() 建立到目标的出站 TCP。
// 域名目标先 resolve4（connect() 不接受域名），逐个候选拨号直到成功。
export async function directConnect(atyp, addr, port) {
  const t0 = Date.now();
  let candidates = [addr];
  if (atyp === ATYP_DOMAIN || !/^([0-9]{1,3}\.){3}[0-9]{1,3}$/.test(addr)) {
    candidates = await resolve4(addr);
    if (!candidates.length) {
      return { error: `dns: no A record for ${addr} (${Date.now() - t0}ms)` };
    }
  }
  let lastErr = "";
  for (const ip of candidates) {
    let socket;
    try {
      socket = connect({ hostname: ip, port });
      await Promise.race([
        socket.opened,
        new Promise((_, rej) => setTimeout(() => rej(new Error("connect timeout")), CONNECT_TIMEOUT_MS)),
      ]);
      return { socket };
    } catch (e) {
      lastErr = e.message || e;
      try { socket?.close(); } catch {}
    }
  }
  return { error: `${lastErr} (${Date.now() - t0}ms, ${candidates.length} candidates)` };
}
