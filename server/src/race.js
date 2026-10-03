// 竞速：6 槽位、交错 120ms（PRD §7.3）。NAT64 已砍（M0 E3：Workers connect() 无
// IPv6 出站），槽位全是 ProxyIP 候选。
//
// 候选来源顺序：Router DO 映射（可选，命中时排第一）→ KV 的 Cron top4 → 内置兜底
// 列表补齐到槽位数。KV/ROUTER 绑定缺席时只剩兜底列表，不影响可用性。
//
// 单槽超时 1.5s、全局 3s 都可以被 env 覆盖；单槽超时直接传进
// connectViaProxyIP，让它自己把中继 socket 关掉——超时回收必须发生在出站连接
// 自己手里，赛道上丢一个 Promise 只能丢引用、关不掉连接。

import {
  dialRelay,
  RELAY_PORT,
  RELAY_TYPE_HTTP_CONNECT,
  RELAY_TYPE_SNI,
  connectViaProxyIP,
  fallbackRelays,
  orderByHealth,
  parseRelay,
} from "./proxyip.js";

export const RACE_SLOTS = 6;
export const RACE_SLOT_TIMEOUT_MS = 1500;
export const RACE_GLOBAL_TIMEOUT_MS = 3000;
export const RACE_STAGGER_MS = 120;

// KV 里 Cron 写的健康结果（last-good-wins）。这里只读前 N 个。
export const KV_RELAY_KEY = "proxyip:top";
export const RACE_KV_TOP = 4;

// 非法/缺省 env 值一律回落到默认：竞速超时写错一个字符不该变成 0ms。
function posEnv(v, dflt) {
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : dflt;
}

export function raceConfig(env = {}) {
  return {
    slots: posEnv(env.RACE_SLOTS, RACE_SLOTS),
    slotMs: posEnv(env.RACE_SLOT_TIMEOUT_MS, RACE_SLOT_TIMEOUT_MS),
    globalMs: posEnv(env.RACE_GLOBAL_TIMEOUT_MS, RACE_GLOBAL_TIMEOUT_MS),
    staggerMs: posEnv(env.RACE_STAGGER_MS, RACE_STAGGER_MS),
    kvTop: posEnv(env.RACE_KV_TOP, RACE_KV_TOP),
  };
}

// parseRelayEntries 宽容解析 Cron 写进 KV 的中继池：JSON 数组或 { relays: [...] }，
// 元素是 "host[:port]" 或 { host, port }。也接受纯文本逐行（订阅源原格式）。
// 解析不出来就当空池——兜底列表接手。
export function parseRelayEntries(text) {
  const out = [];
  const seen = new Set();
  const push = (v) => {
    let r = null;
    if (typeof v === "string") r = parseRelay(v);
    else if (v && typeof v.host === "string") {
      r = parseRelay(`${v.host}:${v.port || RELAY_PORT}`);
      // type 必须跟着走：dialRelay 按 type 分发，session.js 按 type 决定 learn
      // 时机（B1：丢了会恒 undefined，Router DO 在生产永远学不到东西）。
      if (r && (v.type === RELAY_TYPE_HTTP_CONNECT || v.type === RELAY_TYPE_SNI)) r.type = v.type;
    }
    if (!r) return;
    const key = `${r.host}:${r.port}`;
    if (seen.has(key)) return;
    seen.add(key);
    out.push(r);
  };
  const s = String(text ?? "");
  if (s.includes("[") || s.includes("{")) {
    try {
      const j = JSON.parse(s);
      const list = Array.isArray(j) ? j : Array.isArray(j?.relays) ? j.relays : [];
      list.forEach(push);
      return out;
    } catch {
      return out; // 坏 JSON 当空池
    }
  }
  for (const line of s.split(/\r?\n/)) push(line.split("#")[0]);
  return out;
}

// readKvTop 读 KV 的前 n 个中继；KV 缺席或读取失败返回空数组。
async function readKvTop(env, n) {
  try {
    const list = parseRelayEntries(await env.KV.get(KV_RELAY_KEY));
    return list.slice(0, n);
  } catch {
    return [];
  }
}

// buildCandidates 组装有序候选（≤ cfg.slots 个）。
// ctx.candidates 直接覆盖（测试注入）；routerLookup 是可选的异步回调，
// 返回 { host, port } 表示 Router DO 有映射，该候选排第一并标记 viaRouter。
export async function buildCandidates(ctx = {}, cfg = raceConfig(ctx.env)) {
  const out = [];
  const seen = new Set();
  const push = (c, viaRouter) => {
    const key = `${c.host}:${c.port}`;
    if (!c.host || seen.has(key)) return;
    seen.add(key);
    // type 一路带到赢家：session.js 靠它决定 learn 时机（B1：丢了恒 undefined，
    // Router DO 在生产永远学不到东西）。缺省 sni 与 dialRelay 的缺省一致。
    out.push({ host: c.host, port: c.port, type: c.type || "sni", viaRouter: !!viaRouter });
  };
  if (typeof ctx.routerLookup === "function") {
    try {
      const hit = await ctx.routerLookup();
      if (hit && hit.host) push({ ...parseRelay(`${hit.host}:${hit.port || RELAY_PORT}`), type: hit.type }, true);
    } catch {
      // Router DO 不可用：当作未命中，继续用 KV/兜底。
    }
  }
  for (const c of await readKvTop(ctx.env || {}, cfg.kvTop)) push(c, false);
  for (const c of fallbackRelays()) {
    if (out.length >= cfg.slots) break;
    push(c, false);
  }
  return orderByHealth(out.slice(0, cfg.slots));
}

// startRace(ctx, target, routerLookup) —— target 是 { host, port }。
// 返回 { socket, relay, viaRouter } 或 { error }。任一槽成功即回收其余槽位。
export async function startRace(ctx = {}, target = {}, routerLookup) {
  const cfg = raceConfig(ctx.env || {});
  const log = typeof ctx.log === "function" ? ctx.log : () => {};
  const candidates = Array.isArray(ctx.candidates)
    ? orderByHealth(ctx.candidates.slice(0, cfg.slots))
    : await buildCandidates({ env: ctx.env, routerLookup }, cfg);
  if (candidates.length === 0) return { error: "no proxyip candidate available" };

  return new Promise((resolve) => {
    let settled = false;
    let live = candidates.length;
    const slotErrors = new Array(candidates.length).fill("pending");
    const timers = [];
    let globalTimer = null;
    const finish = (r) => {
      if (settled) return;
      settled = true;
      for (const t of timers) clearTimeout(t);
      if (globalTimer) clearTimeout(globalTimer);
      resolve(r);
    };
    const allFailed = () => {
      if (!settled && live === 0) {
        const reasons = slotErrors.map((e, i) => `${candidates[i] ? candidates[i].host : "?"}: ${e}`).join("; ");
        finish({ error: `all ${candidates.length} proxyip exits failed: ${reasons}` });
      }
    };
    globalTimer = setTimeout(() => finish({ error: `race timeout ${cfg.globalMs}ms` }), cfg.globalMs);
    candidates.forEach((cand, i) => {
      timers.push(
        setTimeout(() => {
          dialRelay(cand, target.host, target.port, { timeoutMs: cfg.slotMs })
            .then((r) => {
              live--;
              if (r.error) {
                slotErrors[i] = r.error;
                log(`race slot ${i} ${cand.host}:${cand.port} failed: ${r.error}`);
                allFailed();
                return;
              }
              // 赢家已定后才到达的隧道没有去处，立即回收，不留悬挂出站 socket。
              if (settled) {
                try { r.socket.close(); } catch {}
                return;
              }
              finish({ socket: r.socket, relay: `${cand.host}:${cand.port}`, type: cand.type, viaRouter: cand.viaRouter === true });
            })
            .catch(() => {
              live--;
              allFailed();
            });
        }, i * cfg.staggerMs)
      );
    });
  });
}