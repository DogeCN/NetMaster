// refresh-relays.mjs — ProxyIP 订阅更新器：拉源 → 探测 → 排序 → 写 KV 的 proxyip:top。
//
// 替代 Worker Cron（免费版 cron 只有整点触发且不可靠，实测过）。跑在 GitHub Actions
// runner 上：不受 Workers 的 50 子请求预算约束，也不占 DO 时长。
//
// 用法（CI）：
//   CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ACCOUNT_ID=... node tools/refresh-relays.mjs
//   DRY_RUN=1 node tools/refresh-relays.mjs        只打印要写的内容，不写 KV
//   RELAY_KV_NAMESPACE_ID=... node tools/...        缺省时用 API 按 title=netmaster 查
//
// 退出码：0 = 写成功，**或**本轮全败但已按 last-good-wins 保留上一轮（定时任务不该
// 红着）；1 = 源全挂 / 探测池为空 / 写 KV 失败 / 缺配置。
//
// 消费端是 race.js 的 parseRelayEntries：写进去的字段必须与它逐字段对得上，所以
// 写入格式由 buildTop 单独构造（type 字段 race.js 不读，但 cron.js 写过、运维看得懂）。

import net from "node:net";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";
import {
  fallbackRelays,
  parseRelay,
  RELAY_PORT,
  RELAY_TYPE_HTTP_CONNECT,
} from "../src/proxyip.js";

// ---- 常量 ----

const CF_API = "https://api.cloudflare.com/client/v4";

// 社区源（纯文本 ip:port）。挂了不致命：内置兜底列表接手。
export const SOURCE_URLS = ["https://ipdb.api.030101.xyz/?type=bestproxy"];

// 探测目标固定为一个轻量 HTTP 端点：中继按 CONNECT 隧道转发到这里，能握手成功就
// 说明这条隧道真的能载数据（单纯 TCP connect 成功不代表中继可用，很多 accept 后 RST）。
export const PROBE_TARGET = { host: "cp.cloudflare.com", port: 80 };
export const PROBE_TIMEOUT_MS = 3000;
export const PROBE_CONCURRENCY = 8;

// KV 契约：键名与 race.js 的 KV_RELAY_KEY 一致，取前 4 条（race.js 读前 RACE_KV_TOP 条）。
export const KV_KEY = "proxyip:top";
export const KV_TOP_N = 4;

// 候选上限：消费端只读前 4 条，探测 60+ 条纯属浪费 runner 时间（60/8×3s ≈ 23s
// 已经是上限）。超出时截断并打印，不静默。
export const MAX_CANDIDATES = 60;

// ---- 源解析 ----

// parseRelayList 解析纯文本 ip:port / host:port（# 注释、空行跳过、去重）。
export function parseRelayList(text) {
  const out = [];
  const seen = new Set();
  // 主机名只允许字母数字、点、横线、冒号（IPv6 字面量）。源里混进一行乱码不该占掉
  // 一个探测槽位——MAX_CANDIDATES 只有 60，被垃圾挤掉的是真候选。
  const add = (host, port) => {
    if (!/^[A-Za-z0-9._:-]+$/.test(host)) return;
    const key = `${host}:${port}`;
    if (seen.has(key)) return;
    seen.add(key);
    out.push({ host, port });
  };
  for (const raw of String(text || "").split("\n")) {
    // 行尾注释（"1.2.3.4:1080  # note"）也要剥掉，否则整行被丢
    const line = raw.split("#")[0].trim();
    if (!line) continue;
    // IPv6 字面量自带 ≥2 个冒号，按"最后一个冒号是端口"切一定会切坏
    //（"2001:db8::1" 会变成 host "2001:db8:" + port 1，一条垃圾候选）。
    if ((line.match(/:/g) || []).length >= 2) {
      add(line, RELAY_PORT);
      continue;
    }
    const relay = parseRelay(line);
    if (relay) add(relay.host, relay.port);
  }
  return out;
}

// fetchRelayPool 拉所有远端源，再并上内置兜底列表（两个源互不牵连）。
// 一个池都拼不出来才算失败（退出码 1 的来源之一）。
export async function fetchRelayPool({ urls = SOURCE_URLS, builtin = fallbackRelays(), fetchImpl = fetch, timeoutMs = 10000 } = {}) {
  const sources = [];
  const all = [];
  for (const url of urls) {
    try {
      const r = await fetchImpl(url, { signal: AbortSignal.timeout(timeoutMs) });
      if (!r.ok) throw new Error(`HTTP ${r.status}`);
      const list = parseRelayList(await r.text());
      sources.push({ url, ok: true, count: list.length });
      all.push(...list);
    } catch (e) {
      // 单源失败静默继续：还有内置兜底
      sources.push({ url, ok: false, error: String(e.message || e) });
    }
  }
  sources.push({ url: "builtin:cmliussss", ok: true, count: builtin.length });
  all.push(...builtin);
  const seen = new Set();
  const pool = [];
  for (const r of all) {
    const key = `${r.host}:${r.port}`;
    if (seen.has(key)) continue; // 两个源重叠很常见
    seen.add(key);
    pool.push(r);
  }
  if (!pool.length) throw new Error("no relay candidate from any source");
  return { pool, sources };
}

// filterSelf 剔除回指本 Worker 的条目：回连自身会被平台拒（TCP Loop），留着只占探测
// 槽位。Workers 里能从 cron event 的 URL 拿到 workerHost，Actions 里没有这个事件，
// 所以靠可选环境变量 NETMASTER_WORKER_HOST 给。
export function filterSelf(pool, workerHost) {
  const h = String(workerHost || "").trim().toLowerCase();
  if (!h) return pool;
  return pool.filter((r) => {
    const host = r.host.toLowerCase();
    return host !== h && !host.endsWith(`.${h}`);
  });
}

// ---- 探测 ----

// probeRelay 对单个候选做 TCP 连接 + HTTP CONNECT 握手，限时 timeoutMs（覆盖
// 连接与握手两段，和 cron.js 的语义一致）。返回 { host, port, ok, ms, error }。
export function probeRelay(relay, { timeoutMs = PROBE_TIMEOUT_MS, target = PROBE_TARGET } = {}) {
  const t0 = Date.now();
  return new Promise((resolve) => {
    let sock = null;
    let settled = false;
    let buf = "";
    const finish = (ok, error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try {
        sock?.destroy();
      } catch {}
      resolve({ host: relay.host, port: relay.port, ok, ms: Date.now() - t0, error: error || null });
    };
    const timer = setTimeout(() => finish(false, "timeout"), timeoutMs);
    try {
      sock = net.connect({ host: relay.host, port: relay.port });
    } catch (e) {
      finish(false, String(e.message || e));
      return;
    }
    sock.once("error", (e) => finish(false, e.code || String(e.message)));
    sock.once("close", () => finish(false, "closed before CONNECT response"));
    sock.once("connect", () => {
      sock.write(`CONNECT ${target.host}:${target.port} HTTP/1.1\r\nHost: ${target.host}:${target.port}\r\n\r\n`);
    });
    sock.on("data", (chunk) => {
      buf += chunk.toString("latin1");
      const i = buf.indexOf("\r\n\r\n");
      if (i < 0) {
        if (buf.length > 8192) finish(false, "CONNECT response too large");
        return;
      }
      const status = Number(buf.slice(0, i).split(" ")[1]);
      if (Number.isFinite(status) && status >= 200 && status <= 299) finish(true, null);
      else finish(false, `CONNECT status ${Number.isFinite(status) ? status : "?"}`);
    });
  });
}

// probeAll 固定并发地探测整个池（保持入参顺序，方便对照输出）。
export async function probeAll(pool, { concurrency = PROBE_CONCURRENCY, ...opts } = {}) {
  const out = new Array(pool.length);
  let next = 0;
  const worker = async () => {
    for (;;) {
      const i = next++;
      if (i >= pool.length) return;
      out[i] = await probeRelay(pool[i], opts);
    }
  };
  await Promise.all(Array.from({ length: Math.max(1, Math.min(concurrency, pool.length)) }, worker));
  return out;
}

// ---- 排序 ----

// rankRelays 按 host:port 聚合后排序：成功率 desc，再平均延迟 asc。与 cron.js 的
// rankRelays 语义一致，只是不做 EWMA —— runner 每轮都是全新观测，没有历史可平滑。
export function rankRelays(results) {
  const acc = new Map();
  for (const r of results) {
    const key = `${r.host}:${r.port}`;
    const cur = acc.get(key) || { host: r.host, port: r.port, ms: 0, n: 0, okN: 0 };
    cur.ms += Number(r.ms) || 0;
    cur.n++;
    if (r.ok) cur.okN++;
    acc.set(key, cur);
  }
  return [...acc.values()]
    .map((v) => ({
      host: v.host,
      port: v.port,
      ms: Math.round(v.ms / Math.max(v.n, 1)),
      success: v.okN / Math.max(v.n, 1),
    }))
    .sort((a, b) => {
      // 全败的候选排在后面而不是丢弃：它们可能在下一轮恢复（last-good-wins 的前提）
      if ((a.success > 0) !== (b.success > 0)) return a.success > 0 ? -1 : 1;
      if (a.success !== b.success) return b.success - a.success;
      return a.ms - b.ms;
    });
}

// buildTop 取前 n 条可用项，写入格式与 cron.js 写进 KV 的字段一致。
export function buildTop(ranked, n = KV_TOP_N) {
  return ranked
    .filter((r) => r.success > 0)
    .slice(0, n)
    .map((r) => ({ host: r.host, port: r.port, type: RELAY_TYPE_HTTP_CONNECT, ms: r.ms }));
}

// ---- Cloudflare REST ----

async function cfGet(path, { token, fetchImpl }) {
  const r = await fetchImpl(`${CF_API}${path}`, { headers: { authorization: `Bearer ${token}` } });
  if (!r.ok) throw new Error(`GET ${path} -> HTTP ${r.status}: ${(await r.text()).slice(0, 200)}`);
  return r;
}

// resolveNamespace 优先用显式 id，否则按 title=netmaster 查（与 deploy.yml 同口径）。
export async function resolveNamespace({ token, account, explicit = "", fetchImpl = fetch }) {
  if (explicit) return explicit;
  const r = await cfGet(`/accounts/${account}/storage/kv/namespaces`, { token, fetchImpl });
  const list = JSON.parse(await r.text()).result || [];
  const ns = list.find((x) => x.title === "netmaster") || list[0];
  if (!ns) throw new Error("no KV namespace found in this account");
  return ns.id;
}

// kvPut 覆写一个 key。键名里的 ':' 必须编码（proxyip%3Atop），否则 Cloudflare 会
// 把它当成路由的一部分。
export async function kvPut({ token, account, ns, key = KV_KEY, value, fetchImpl = fetch }) {
  const url = `${CF_API}/accounts/${account}/storage/kv/namespaces/${ns}/values/${encodeURIComponent(key)}`;
  const r = await fetchImpl(url, {
    method: "PUT",
    headers: { authorization: `Bearer ${token}`, "content-type": "text/plain;charset=UTF-8" },
    body: value,
  });
  if (!r.ok) throw new Error(`PUT ${key} -> HTTP ${r.status}: ${(await r.text()).slice(0, 200)}`);
  return { url, bytes: String(value).length };
}

// ---- 编排 ----

// refresh 是主体，依赖全部可注入（测试用本地桩，不碰真 KV / 真网络）。
// 返回 { wrote, top, pool, results, reason }，由 CLI 决定退出码。
export async function refresh(deps = {}) {
  const {
    env = process.env,
    log = (m) => console.log(m),
    fetchImpl = fetch,
    probe = probeAll,
    put = kvPut,
    resolveNs = resolveNamespace,
    builtin = fallbackRelays(),
    urls = SOURCE_URLS,
    maxCandidates = MAX_CANDIDATES,
    concurrency = PROBE_CONCURRENCY,
    timeoutMs = PROBE_TIMEOUT_MS,
    target = PROBE_TARGET,
  } = deps;
  // DRY_RUN 的取值要兼容两种写法：GH workflow_dispatch 的 boolean input 渲染成
  // "true"/"false"，本地习惯写 DRY_RUN=1。
  const dryRun = /^(1|true)$/i.test(String(env.DRY_RUN || ""));
  const capEnv = Number(env.RELAY_MAX_CANDIDATES);
  const cap = Number.isFinite(capEnv) && capEnv > 0 ? capEnv : maxCandidates;
  const token = env.CLOUDFLARE_API_TOKEN || "";
  const account = env.CLOUDFLARE_ACCOUNT_ID || "";

  const { pool: rawPool, sources } = await fetchRelayPool({ urls, builtin, fetchImpl });
  for (const s of sources) log(`source ${s.url}: ${s.ok ? `${s.count} entries` : `FAILED (${s.error})`}`);
  const selfFiltered = filterSelf(rawPool, env.NETMASTER_WORKER_HOST);
  if (selfFiltered.length !== rawPool.length) log(`filtered ${rawPool.length - selfFiltered.length} self-referencing entries`);
  const pool = selfFiltered.slice(0, cap);
  if (selfFiltered.length > pool.length) log(`candidates truncated: ${selfFiltered.length} -> ${pool.length} (RELAY_MAX_CANDIDATES)`);

  log(`probing ${pool.length} relays (concurrency ${concurrency}, timeout ${timeoutMs}ms, target ${target.host}:${target.port})`);
  const results = await probe(pool, { concurrency, timeoutMs, target });
  const ranked = rankRelays(results);
  const okCount = ranked.filter((r) => r.success > 0).length;
  log(`probes: ${okCount}/${ranked.length} usable`);
  // 失败原因一并打出来：run 页面就是唯一的排障现场，"fail" 而没有理由等于没信息
  const failure = new Map(results.map((r) => [`${r.host}:${r.port}`, r.error || ""]));
  for (const r of ranked) {
    const why = r.success > 0 ? "" : ` (${failure.get(`${r.host}:${r.port}`) || "unknown"})`;
    log(`  ${r.success > 0 ? "ok  " : "fail"} ${r.host}:${r.port} ${r.ms}ms${why}`);
  }

  const top = buildTop(ranked, KV_TOP_N);
  const payload = JSON.stringify(top);

  // last-good-wins：本轮全败就不写，保留上一轮已验证过的池子。
  if (!top.length) {
    const reason = `no usable relay this round (${results.length} probed) — keeping the previous ${KV_KEY}`;
    log(`last-good-wins: ${reason}`);
    return { wrote: false, dryRun, top, pool, results, reason };
  }
  log(`${dryRun ? "dry-run: would write" : "writing"} ${KV_KEY} = ${payload}`);
  if (dryRun) return { wrote: false, dryRun, top, pool, results, reason: "dry run" };

  if (!token || !account) throw new Error("CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID are required to write KV (or use DRY_RUN=1)");
  const ns = await resolveNs({ token, account, explicit: env.RELAY_KV_NAMESPACE_ID || "", fetchImpl });
  const res = await put({ token, account, ns, key: KV_KEY, value: payload, fetchImpl });
  log(`KV ${KV_KEY} written to namespace ${ns} (${res.bytes} bytes)`);
  return { wrote: true, dryRun, top, pool, results, ns, reason: "" };
}

// CLI 入口：只有失败才给非零退出码——定时任务红了会让人习惯性忽略。
// 判定用绝对路径比对而不是 endsWith("refresh-relays.mjs")：单测文件同名
// （test/refresh-relays.mjs）import 本模块时会把 CLI 一起跑起来。
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const r = await refresh();
    console.log(`refresh-relays: ${r.wrote ? "wrote " + r.top.length + " relays" : "kept previous (" + r.reason + ")"}`);
    process.exit(0);
  } catch (e) {
    console.error(`refresh-relays: ${e.message || e}`);
    process.exit(1);
  }
}