// profile.mjs —— 把客户端与服务端的 profile 合成一条时间线。
//
// ## 它要防的那件事
//
// 比"优化了 7.2s → 5.1s"更糟的是**比错了**。本项目已经吃过一次：入口池改动前
// 用了 `nodes[:64]` 随机截断，于是两次跑的节点池根本不是一回事，结论作废。
// 所以这个工具的第一职责不是画图，而是**在两次 profile 不可比时明说不可比**。
//
// ## 指纹是什么
//
// sha256(排序后的 facts)，12 位十六进制。facts 覆盖：节点池身份、tunnels、ech、
// insecure、rules-file、frag 参数、build。两端各算自己那份，合并时逐项 diff。
// 只要有一项不同，就拒绝给出"谁更快"的结论 —— 差异可能全来自配置而不是代码。
//
// ## 用法
//
//   node tools/profile.mjs compare a.json b.json            # 客户端两次运行
//   node tools/profile.mjs merge    client.json server.json # 双端一条时间线
//   node tools/profile.mjs fingerprint doc.json             # 只算指纹
//
// 服务端记录从 KV 取（`profile:<target>:<minute>`，TTL 1 小时，键里的 target 是
// profile.js 的 targetOf 截到 64 字符的目标串、minute 是分钟数）：
//
//   npx wrangler kv key list --namespace-id <id> --remote | grep '"profile:'
//   npx wrangler kv key get 'profile:<target>:<minute>' --namespace-id <id> --remote > server.json
//
// server.json 可以是取下来的单条记录，或多条记录组成的数组（同一轮的多个目标）。

import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';

const argv = process.argv.slice(2);
const cmd = argv[0];

// ---- 指纹 ---------------------------------------------------------------

export function fingerprint(facts) {
  const pairs = Object.entries(facts || {}).map(([k, v]) => `${k}=${v}`).sort();
  return createHash('sha256').update(pairs.join('\n')).digest('hex').slice(0, 12);
}

// diffFacts 返回逐项差异，格式 [key, "old → new"]；缺失的一侧标 "(缺)"。
export function diffFacts(a, b) {
  const keys = [...new Set([...Object.keys(a || {}), ...Object.keys(b || {})])].sort();
  const out = [];
  for (const k of keys) {
    const av = a?.[k], bv = b?.[k];
    if (av === bv) continue;
    out.push([k, `${av === undefined ? '(缺)' : av} → ${bv === undefined ? '(缺)' : bv}`]);
  }
  return out;
}

// ---- 读入 ---------------------------------------------------------------

function readJSON(p) {
  return JSON.parse(readFileSync(p, 'utf8'));
}

// 客户端 JSON 是 internal/profile 写出的形状：{ facts: {...}, spans: [...], total }
// 服务端 KV 记录是 profile.js 的形状：{ v, target, at, spans, marks, counts }
function isServerRecord(x) {
  return x && typeof x === 'object' && ('target' in x || 'counts' in x) && !('facts' in x);
}

function normClient(doc) {
  const facts = doc.facts || {};
  // 客户端的 Span 序列化成 `dur`（Go 的 time.Duration = **纳秒**整数），而服务端
  // profile.js 用的是 `ms`。这里统一折成 ms —— 不折的话客户端每个阶段都会显示成
  // 0，一条时间线的两半就没法放一起看了。
  const spans = (doc.spans || []).map((s) => ({
    name: s.name,
    ms: Math.round((s.dur || 0) / 1e6),
    budgetMs: s.budget ? Math.round(s.budget / 1e6) : 0,
    attrs: s.attrs || [],
  }));
  return { side: 'client', facts, spans, fingerprint: doc.fingerprint || fingerprint(facts) };
}

function normServer(doc) {
  const facts = { build: 'worker', target: doc.target || 'session', at: doc.at || '' };
  return {
    side: 'server',
    target: doc.target || 'session',
    facts,
    spans: doc.spans || [],
    marks: doc.marks || [],
    counts: doc.counts || {},
    // 服务端侧没有 facts 可算指纹（它只知道目标和时刻），所以指纹只对客户端两次
    // 运行之间有意义。merge 时不比指纹，改比"目标能不能对上"。
    fingerprint: null,
  };
}

function load(p) {
  const doc = readJSON(p);
  return isServerRecord(doc) ? normServer(doc) : normClient(doc);
}

// ---- 输出 ---------------------------------------------------------------

function bar(ms, scale) {
  const width = Math.max(1, Math.min(40, Math.round((ms / scale) * 40)));
  return '#'.repeat(width);
}

function printSpans(label, spans, scale) {
  if (!spans.length) {
    console.log(`  (${label}: 无记录 —— 服务端侧通常是没开 PROFILE，或连接还没关)`);
    return;
  }
  const byName = new Map();
  for (const s of spans) {
    const cur = byName.get(s.name) || { ms: 0, n: 0, max: 0 };
    cur.ms += s.ms || 0;
    cur.n += 1;
    cur.max = Math.max(cur.max, s.ms || 0);
    byName.set(s.name, cur);
  }
  console.log(`  ${label}`);
  for (const [name, v] of [...byName].sort((a, b) => b[1].ms - a[1].ms)) {
    console.log(
      `    ${name.padEnd(20)} total ${String(v.ms).padStart(6)}ms  ` +
      `max ${String(v.max).padStart(6)}ms  n=${String(v.n).padStart(3)}  ${bar(v.ms, scale)}`
    );
  }
}

// ---- 子命令 -------------------------------------------------------------

function cmdCompare() {
  const [pa, pb] = argv.slice(1);
  if (!pa || !pb) die('usage: profile.mjs compare <a.json> <b.json>');
  const A = load(pa), B = load(pb);

  console.log(`A: ${pa}`);
  console.log(`B: ${pb}`);
  console.log(`指纹  A=${A.fingerprint || fingerprint(A.facts)}  B=${B.fingerprint || fingerprint(B.facts)}`);

  const d = diffFacts(A.facts, B.facts);
  if (d.length) {
    console.log('\n⛔ 两次 profile **不可比**，先看这些差异：');
    for (const [k, v] of d) console.log(`   ${k}: ${v}`);
    console.log('\n指纹不同就意味着差异可能全部来自配置而不是代码。');
    console.log('要对齐这些 fact 后重跑，或者别把这次的数字当成优化结论。');
    process.exitCode = 2;
    return;
  }

  console.log('\n✅ 指纹一致，可以比。');
  const scale = Math.max(1, ...A.spans.map((s) => s.ms || 0), ...B.spans.map((s) => s.ms || 0));
  printSpans('A 各阶段', A.spans, scale);
  printSpans('B 各阶段', B.spans, scale);
}

function cmdMerge() {
  const [pc, ps] = argv.slice(1);
  if (!pc || !ps) die('usage: profile.mjs merge <client.json> <server.json>');
  const C = load(pc);
  const serverDocs = Array.isArray(readJSON(ps)) ? readJSON(ps) : [readJSON(ps)];
  const S = serverDocs.map(normServer);

  console.log('=== 双端全链路 ===\n');
  console.log(`客户端指纹: ${C.fingerprint}`);
  console.log(`服务端记录: ${S.length} 条（${S.map((s) => s.target).join(', ')}）`);
  console.log();

  const scale = Math.max(
    1,
    ...C.spans.map((s) => s.ms || 0),
    ...S.flatMap((s) => s.spans.map((x) => x.ms || 0))
  );
  printSpans('客户端', C.spans, scale);
  for (const s of S) printSpans(`服务端 ${s.target}`, s.spans, scale);

  // 关联：客户端的 facts 里如果有目标，两侧靠 (目标, 时间窗) 对齐。
  console.log('\n=== 关联 ===');
  console.log('协议首帧没有余量塞 trace id，两侧靠 (目标, 时间窗) 关联（见 profile.js 头部）。');
  console.log('因此：客户端 facts 里的目标必须出现在服务端记录的 target 里，否则对不上。');
  const cTarget = Object.entries(C.facts).find(([k]) => /host|target|url/i.test(k));
  if (cTarget) {
    const hit = S.some((s) => String(s.target).includes(String(cTarget[1])));
    console.log(`  客户端 ${cTarget[0]}=${cTarget[1]} → 服务端 ${hit ? '匹配到' : '⚠️ 没匹配到'}`);
    if (!hit) {
      console.log('  ⚠️ 对不上时不要把两侧数字相减当"服务端耗时" —— 那多半是关联错了。');
      process.exitCode = 2;
    }
  }

  // 出口阶梯分布：哪一级被用得最多。
  const rungs = S.flatMap((s) => Object.entries(s.counts || {}).filter(([k]) => k === 'exit.rung'));
  if (rungs.length) {
    const agg = new Map();
    for (const [, v] of rungs) agg.set(Number(v), (agg.get(Number(v)) || 0) + 1);
    console.log('\n=== 出口阶梯命中分布（0=直连 1=会话缓存 2=Router 3=竞速）===');
    for (const [k, v] of [...agg].sort((a, b) => a[0] - b[0])) console.log(`  rung ${k}: ${v} 次`);
    const dominated = [...agg].sort((a, b) => b[1] - a[1])[0];
    console.log(`  最常走的是 rung ${dominated[0]}（${dominated[1]} 次）—— ` +
      '若期望走 0（直连）却总落在 3（竞速），说明"直连被判死"的判断偏保守。');
  }
}

function die(msg) {
  console.error(msg);
  process.exit(1);
}

if (cmd === 'compare') cmdCompare();
else if (cmd === 'merge') cmdMerge();
else if (cmd === 'fingerprint') console.log(fingerprint(readJSON(argv[1]).facts ? Object.fromEntries(readJSON(argv[1]).facts) : {}));
else die('usage: profile.mjs <compare|merge|fingerprint> ...');