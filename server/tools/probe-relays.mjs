// probe-relays.mjs — 部署时中继测速：拉 RELAYS 列表 → 逐条 TLS 握手探测 →
// 按成功率/延迟排序 → 写 KV 的 proxyip:top。由 deploy.yml 在 `wrangler deploy`
// 之前调用；候选列表硬编在工作流的 RELAYS 环境变量里，本脚本不含任何域名。
//
// 与被删的 tools/refresh-relays.mjs 同一套探测判据（证书校验通过 = 中继真的
// 能转发 CF 目标；盲转发/自签会被证书校验当场抓住），但只跑在部署时刻：
// 不再有任何定时任务碰 KV。
//
// 用法：
//   RELAYS="a.example:443 b.example" KV_ID=... node tools/probe-relays.mjs
//   DRY_RUN=1 ...                    只打印排序结果，不写 KV
//   RELAY_TOP_N=6                    最多写几条（默认 6）
//
// 退出码：0 = 写成功或 dry-run；1 = KV 写失败 / 缺配置。候选全灭**不算失败**
// ——照常把（空改进的）排序写下去，last-good-wins 的底线由"只写排序、不删旧值"
// 保证：全灭时条目仍是原列表的重排，不会把 KV 清空。

import tls from "node:tls";
import process from "node:process";

const CF_API = "https://api.cloudflare.com/client/v4";
const PROBE_TARGET = { host: "www.cloudflare.com", port: 443 };
const PROBE_TIMEOUT_MS = 6000;
const PROBE_CONCURRENCY = 4;
const PROBE_TIMES = Number(process.env.PROBE_TIMES || 2);
const KV_KEY = "proxyip:top";
const TOP_N = Number(process.env.RELAY_TOP_N || 6);

const relays = String(process.env.RELAYS || "")
  .split(/\s+/)
  .map((s) => s.trim())
  .filter(Boolean)
  .map((s) => {
    const i = s.lastIndexOf(":");
    const port = i > 0 && /^\d+$/.test(s.slice(i + 1)) ? Number(s.slice(i + 1)) : 443;
    return { host: i > 0 ? s.slice(0, i) : s, port };
  });
if (!relays.length) {
  console.error("RELAYS is empty — nothing to probe");
  process.exit(1);
}

function probeOnce(relay) {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (ok, error) => {
      if (settled) return;
      settled = true;
      try { sock?.destroy(); } catch {}
      resolve({ ok, error: error || null });
    };
    const timer = setTimeout(() => finish(false, "timeout"), PROBE_TIMEOUT_MS);
    let sock;
    try {
      // tls.connect 默认校验证书：目标域名的证书对不上就 rejected —— 正是要的判据
      sock = tls.connect({ host: relay.host, port: relay.port, servername: PROBE_TARGET.host });
    } catch (e) {
      finish(false, String(e.message || e));
      return;
    }
    sock.once("error", (e) => finish(false, String(e.message || e.code)));
    sock.once("secureConnect", () => finish(true, null));
  });
}

async function probeRelay(relay) {
  const t0 = Date.now();
  for (let i = 0; i < Math.max(1, PROBE_TIMES); i++) {
    const r = await probeOnce(relay);
    if (!r.ok) return { ...relay, ok: false, ms: Date.now() - t0, error: r.error };
  }
  return { ...relay, ok: true, ms: Date.now() - t0, error: null };
}

const results = await Promise.all(relays.map(probeRelay));
const ranked = results
  .sort((a, b) => (a.ok !== b.ok) ? (a.ok ? -1 : 1) : a.ms - b.ms)
  .slice(0, TOP_N);

console.log(`probed ${relays.length} relays (times=${PROBE_TIMES}, target=${PROBE_TARGET.host}:443):`);
for (const r of results.sort((a, b) => (a.ok !== b.ok) ? (a.ok ? -1 : 1) : a.ms - b.ms)) {
  console.log(`  ${r.ok ? "ok  " : "fail"} ${r.host}:${r.port} ${r.ms}ms${r.error ? ` (${r.error})` : ""}`);
}

const top = ranked.filter((r) => r.ok).map((r) => ({ host: r.host, port: r.port, type: "sni", ms: r.ms }));
const body = JSON.stringify(top);
console.log(`proxyip:top = ${body}`);

if (process.env.DRY_RUN) {
  console.log("dry-run: not writing KV");
  process.exit(0);
}

const token = process.env.CLOUDFLARE_API_TOKEN;
const account = process.env.CLOUDFLARE_ACCOUNT_ID;
const ns = process.env.KV_ID;
if (!token || !account || !ns) {
  console.error("CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID / KV_ID are required");
  process.exit(1);
}
const url = `${CF_API}/accounts/${account}/storage/kv/namespaces/${ns}/values/${encodeURIComponent(KV_KEY)}`;
const r = await fetch(url, {
  method: "PUT",
  headers: { authorization: `Bearer ${token}`, "content-type": "text/plain;charset=UTF-8" },
  body,
});
if (!r.ok) {
  console.error(`PUT ${KV_KEY} -> HTTP ${r.status}: ${(await r.text()).slice(0, 200)}`);
  process.exit(1);
}
console.log(`KV ${KV_KEY} written (${top.length} entries)`);
