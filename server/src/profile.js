// profile.js —— Session DO 内的分段耗时采集。
//
// ## 为什么必须写 KV，而不是打日志
//
// m0-findings.md E9 实测：**DO 内的 console 输出在 `wrangler tail` 上完全不可见**。
// 所以"加一行 console 看看服务端慢在哪"这条路是断的 —— 不是忘了加，是加了也
// 看不见。Session DO 里唯一能把数据带出来的通道是 KV（session.js 里
// `debug:lastExit` 就是这么做的）。
//
// ## 为什么按目标哈希 + 分钟分桶
//
// 协议首帧是 AUTH|TS|STREAM_ID|ATYP|ADDR|PORT，没有余量塞一个 trace id，塞了就是
// 破坏性协议变更。而两侧本来就都带着目标：客户端知道 host，服务端能算出 targetHash。
// 用"目标 + 时间窗"做关联已经足够回答"这一条连接的时间花在服务端哪一段"。
//
// 分桶到分钟是为了让一次验证跑出的多条记录并存 —— 否则同一目标后写的会覆盖先写的，
// 一次验证只剩最后一个样本。
//
// ## 成本
//
// KV 写入要花配额，所以默认关闭，并且每个会话最多 flush 若干次。关闭时 makeProfiler
// 返回的对象只有一个 enabled=false 的字段判断，热路径上不多花什么。

const KEY_PREFIX = "profile";
const TTL_SECONDS = 3600;

// flushesPerSession 限制一个会话最多写几次 KV。一条连接一生通常只 flush 一次
// （关闭时），偶发的重连与预算回收会多几次 —— 设上限是为了防住"客户端疯狂重连"
// 把 KV 写爆，那正是最不该无限花费的场景。
const FLUSHES_PER_SESSION = 3;

// targetOf 取用于分桶的稳定标识。没有目标时退到 "session"。
function targetOf(target) {
  if (!target) return "session";
  return String(target).slice(0, 64);
}

// makeProfiler 造一个采集器。env.PROFILE === '1' 时才真正启用。
export function makeProfiler(env, opts = {}) {
  const enabled = String(env?.PROFILE || "") === "1" && Boolean(env?.KV);
  const p = {
    enabled,
    target: targetOf(opts.target),
    spans: [],
    marks: [],
    counts: {},
    flushes: 0,
  };

  if (!enabled) {
    // 关闭时把方法换成空实现：调用点不必写 if，也就不会有人因为"看着像无用代码"
    // 把它删掉。
    p.begin = () => () => {};
    p.mark = () => {};
    p.count = () => {};
    return p;
  }

  p.begin = (name, attrs) => {
    const start = Date.now();
    return () => {
      p.spans.push({ name, ms: Date.now() - start, attrs: attrs || undefined });
    };
  };
  p.mark = (name, attrs) => {
    p.marks.push({ name, ms: Date.now(), attrs: attrs || undefined });
  };
  p.count = (key, by = 1) => {
    p.counts[key] = (p.counts[key] || 0) + by;
  };
  return p;
}

// key 本次记录落在 KV 的哪个键。
export function profileKey(target, at = Date.now()) {
  const minute = Math.floor(at / 60000);
  return `${KEY_PREFIX}:${target}:${minute}`;
}

// flush 把已采集的内容写进 KV。fire-and-forget：它绝不能拖慢或打断请求路径，
// 失败也不重试 —— 观测数据本身没有值得为之重试的价值。
//
// budget 是**可选的会话级共享配额**（{ left: n }）。为什么需要它：一条 WS 连接会
// 承载很多目标，如果每个目标一个采集器、各自按 FLUSHES_PER_SESSION 算，一次首屏
// 就能写掉几十次 KV —— 而"客户端疯狂开页面"恰恰是最不该无限花费的场景。
// 传了 budget 就以它为准，flushesPerSession 只在单采集器时兜底。
export async function flush(p, env, budget) {
  if (!p || !p.enabled) return;
  if (budget) {
    if (budget.left <= 0) return;
    budget.left -= 1;
  } else if (p.flushes >= FLUSHES_PER_SESSION) {
    return;
  }
  p.flushes += 1;
  const record = {
    v: 1,
    target: p.target,
    at: new Date().toISOString(),
    spans: p.spans,
    marks: p.marks,
    counts: p.counts,
  };
  try {
    await env.KV.put(profileKey(p.target), JSON.stringify(record), {
      expirationTtl: TTL_SECONDS,
    });
  } catch {
    // KV 缺席、配额不足、序列化失败 —— 一律吞掉。profile 是排障工具，
    // 它坏了不该影响转发。
  }
  // 写完清空，避免同一个会话反复把同一批数据重写一遍。
  p.spans = [];
  p.marks = [];
}

// summarize 把一份记录压成"各阶段总耗时 / 次数"，供合并工具直接比较。
export function summarize(record) {
  const by = new Map();
  for (const s of record.spans || []) {
    const cur = by.get(s.name) || { ms: 0, n: 0, max: 0 };
    cur.ms += s.ms || 0;
    cur.n += 1;
    if ((s.ms || 0) > cur.max) cur.max = s.ms || 0;
    by.set(s.name, cur);
  }
  return {
    target: record.target,
    at: record.at,
    stages: Object.fromEntries(by),
    counts: record.counts || {},
    marks: (record.marks || []).map((m) => m.name),
  };
}
