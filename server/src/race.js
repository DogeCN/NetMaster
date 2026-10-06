// 竞速：6 槽位、交错 120ms（PRD §7.3）。NAT64 已砍（M0 E3：Workers connect() 无
// IPv6 出站），槽位全是 ProxyIP 候选。
//
// 候选来源：内置硬编列表（proxyip.js，实测幸存的三条 CMLiussss）按序补齐到槽位数。
// Router DO 映射与 KV 的 Cron top4 已随动态更新一起砍掉（2026-10-06）——会话内的
// 出口亲和由 session.js 的 egress 缓存承担，跨会话不再记忆。
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
} from "./proxyip.js";

export const RACE_SLOTS = 6;
export const RACE_SLOT_TIMEOUT_MS = 1500;
export const RACE_GLOBAL_TIMEOUT_MS = 3000;
export const RACE_STAGGER_MS = 120;

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
  };
}

// buildCandidates 组装有序候选（≤ cfg.slots 个）：内置硬编列表按序补齐。
// ctx.candidates 直接覆盖（测试注入）。
export async function buildCandidates(ctx = {}, cfg = raceConfig(ctx.env)) {
  const out = [];
  const seen = new Set();
  const push = (c) => {
    const key = `${c.host}:${c.port}`;
    if (!c.host || seen.has(key)) return;
    seen.add(key);
    // type 一路带到赢家：session.js 按它决定 learn 时机。缺省 sni 与 dialRelay 的缺省一致。
    out.push({ host: c.host, port: c.port, type: c.type || "sni" });
  };
  for (const c of fallbackRelays()) {
    if (out.length >= cfg.slots) break;
    push(c);
  }
  return orderByHealth(out.slice(0, cfg.slots));
}

// startRace(ctx, target) —— target 是 { host, port }。
// 返回 { socket, relay } 或 { error }。任一槽成功即回收其余槽位。
export async function startRace(ctx = {}, target = {}) {
  const cfg = raceConfig(ctx.env || {});
  const log = typeof ctx.log === "function" ? ctx.log : () => {};
  const candidates = Array.isArray(ctx.candidates)
    ? orderByHealth(ctx.candidates.slice(0, cfg.slots))
    : await buildCandidates({ env: ctx.env }, cfg);
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
              finish({ socket: r.socket, relay: `${cand.host}:${cand.port}`, type: cand.type });
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