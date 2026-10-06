// 顺序出口选择：候选按"上次谁赢谁排前"的顺序**逐个**试，不再竞速。
//
// 为什么放弃竞速：竞速的收益是"新目标首建连最快"，代价是最坏 6 条并发出站
// socket + 等量子请求预算。候选池缩到个位数、且顺序本身带记忆（成功提前、
// 失败置后）之后，第一个候选几乎总是上次的赢家 —— 并发换来的那点首包时间，
// 抵不过多烧的预算与 socket。顺序执行的代价是"首候选死了要逐个等下去"，
// 由单条时限兜底（orderConfig.slotMs）。
//
// 顺序的权威副本在 KV（proxyip:top，部署工作流测速排序后写入）；运行期的
// 变化就地生效并写回 KV —— **只在顺序真的变了才写**，写回有会话级预算
// （session.js 的 persistRelayOrder）。同一 isolate/会话内的重复目标走
// session.js 的 egress 缓存，根本不进这里。

import { dialRelay, RELAY_PORT, parseRelay } from './proxyip.js';

// 最多带多少条候选进尝试序列。部署工作流可能测出更多可用中继，这里截断。
export const ORDER_SLOTS = 6;

// 单条候选的尝试上限。SNI 型中继是跨洋 TCP+TLS，2-3s 属正常（worker 实测
// CMLiussss 从 GH runner 连接要 2.4s），取 5s：足够慢中继完成握手，又不至于
// 让"全灭"拖成几分钟。
export const ORDER_SLOT_TIMEOUT_MS = 5000;

// KV 键：部署工作流写入的有序候选（[{host,port,type,ms}, ...]）。
export const KV_RELAY_KEY = "proxyip:top";

// 非法/缺省 env 值一律回落到默认：超时写错一个字符不该变成 0ms。
function posEnv(v, dflt) {
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : dflt;
}

export function orderConfig(env = {}) {
  return {
    slots: posEnv(env.ORDER_SLOTS, ORDER_SLOTS),
    slotMs: posEnv(env.ORDER_SLOT_TIMEOUT_MS, ORDER_SLOT_TIMEOUT_MS),
  };
}

// parseRelayEntries 宽容解析 KV 里的有序候选：JSON 数组或 { relays: [...] }，
// 元素是 "host[:port]" 或 { host, port }。也接受纯文本逐行（订阅源原格式）。
// 解析不出来就当空池 —— 上层报 no candidate，客户端看到 0x03。
export function parseRelayEntries(text) {
  const out = [];
  const seen = new Set();
  const push = (v) => {
    let r = null;
    if (typeof v === "string") r = parseRelay(v);
    else if (v && typeof v.host === "string") {
      r = parseRelay(`${v.host}:${v.port || RELAY_PORT}`);
      // type 必须跟着走：dialRelay 按 type 分发握手方式。
      if (r && (v.type === "http-connect" || v.type === "sni")) r.type = v.type;
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

// readOrder 读 KV 里的有序候选，截到 slots 条。KV 缺席或读取失败返回空数组 ——
// 空的代价是 dialOrdered 报 no candidate，与"中继全灭"同途。
export async function readOrder(env, slots = ORDER_SLOTS) {
  try {
    return parseRelayEntries(await env.KV.get(KV_RELAY_KEY)).slice(0, slots);
  } catch {
    return [];
  }
}

// dialOrdered 顺序尝试候选，返回 { socket, relay, type } 或 { error }。
//
// 顺序的就地维护：
//   - 成功且不是第一位 → 提前到首位，onReorder(order)（调用方决定是否写 KV）；
//   - 失败 → 踢到末尾，继续试下一个（下一个就在当前位置，所以游标不动）。
// ctx.order 是调用方的会话内存副本（ getter/setter 皆可），加载与每次重排都写
// 回它；ctx.candidates 直接覆盖（测试注入）。
export async function dialOrdered(ctx = {}, target = {}) {
  const cfg = orderConfig(ctx.env);
  const log = typeof ctx.log === "function" ? ctx.log : () => {};
  let order;
  if (Array.isArray(ctx.candidates)) {
    order = ctx.candidates.slice(0, cfg.slots);
  } else if (ctx.order) {
    order = ctx.order;
  } else {
    order = await readOrder(ctx.env, cfg.slots);
    if (ctx.order !== undefined) ctx.order = order; // 经 setter 回填会话内存
  }
  if (!order.length) return { error: "no proxyip candidate available" };

  // 每条候选**只试一次**：失败踢到末尾后游标不动（下一条顶上来），但尝试计数
  // 照加。没有这个计数，全灭场景会永远转圈 —— 刚失败被踢到末尾的候选轮一圈
  // 又回到眼前（这是实现当场踩到的死循环，测试套件挂住才暴露的）。
  const total = order.length;
  let changed = false;
  for (let i = 0, tried = 0; tried < total; tried++) {
    const cand = order[i];
    const r = await dialRelay(cand, target.host, target.port, { timeoutMs: cfg.slotMs });
    if (!r.error) {
      if (i !== 0) {
        order.splice(i, 1);
        order.unshift(cand);
        changed = true;
      }
      if (changed && typeof ctx.onReorder === "function") ctx.onReorder(order);
      return { socket: r.socket, relay: `${cand.host}:${cand.port}`, type: cand.type || "sni" };
    }
    log(`relay ${cand.host}:${cand.port} failed: ${r.error}`);
    if (i < total - 1) {
      order.splice(i, 1);
      order.push(cand);
      changed = true;
      // 游标不动：踢到末尾后，原来 i+1 的候选现在就在 i 上。
    }
  }
  // 全灭：顺序也已经转过了一圈，照常上报 —— 下个流从"最久没失败的"开始。
  if (changed && typeof ctx.onReorder === "function") ctx.onReorder(order);
  return { error: `all ${total} proxyip exits failed` };
}
