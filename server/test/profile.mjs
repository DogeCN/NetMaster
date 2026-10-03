// profile 接线的钉子。
//
// 为什么单独一个文件：这份代码最危险的形态不是"写错"，而是"看起来在那儿"。
// profile.js 建好了、import 写好了、build.mjs 也把它打进 bundle 了，而 makeProfiler
// 一次都没被调用 —— 那正是本项目被咬过两次的形态（"空闲回收已实施"、"ECH 默认隐
// SNI"：代码在、行为没接）。所以判据必须是**KV 里真的出现了一条记录**，而不是
// "源���里有这些函数名"。
//
// 这里用的是 regressions.mjs 那套源码抽取：把 session.js 里关心的方法原样抽出来
// 贴进 new Function。理由和那边一样 —— session.js 依赖 build.mjs 的拼接作用域
// （import 已被剥掉），整文件没法在 Node 里 import；抽取则让断言跑在**真实源码**上。

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
// 真的 profile.js，不是替身：这一套要验的就是"真采集器 + 真 closeAll"接在一起
// 会不会写 KV。它不 import 任何 cloudflare:* 模块，所以可以直接在 Node 里加载。
import { makeProfiler, flush } from '../src/profile.js';

// extract 按方法名抽出 session.js 里的方法源码（带花括号配平）。
// async 方法的前缀也要认 —— openExit/flushProfile 都是 async 的。
function extractMethod(src, name) {
  let start = src.indexOf(`\n  async ${name}(`);
  if (start === -1) start = src.indexOf(`\n  ${name}(`);
  assert.notEqual(start, -1, `cannot find ${name} in session.js — update this test`);
  let i = src.indexOf('{', start);
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) {
      i++;
      break;
    }
  }
  return src.slice(start, i).replace(/^ {2}/gm, '');
}

// asFunctionDecl 把抽出来的类方法改写成函数声明 —— 裸的 `foo() {}` 在函数作用域里
// 是非法的（regressions.mjs 对 openExit 做的是同一件事）。
// 抽取结果的第一个字符是换行（切片从 `\n  foo(` 那个换行开始），所以正则要容许
// 前导空白，并把缩进原样带回去。
function asFunctionDecl(methodSrc, name) {
  const re = new RegExp(`^(\\s*)(async )?${name}\\(`);
  const m = methodSrc.match(re);
  assert.ok(m, `unexpected shape for ${name} — update this test`);
  return methodSrc.replace(re, `${m[1]}${m[2] || ''}function ${name}(`);
}

function makeSession(env) {
  const src = readFileSync(new URL('../src/session.js', import.meta.url), 'utf8');
  const profFor = asFunctionDecl(extractMethod(src, 'profFor'), 'profFor');
  const flushProfile = asFunctionDecl(extractMethod(src, 'flushProfile'), 'flushProfile');
  const closeAll = asFunctionDecl(extractMethod(src, 'closeAll'), 'closeAll');

  // makeProfiler / flush 来自上面的真 profile.js import。
  const factory = new Function(
    'makeProfiler', 'flush', 'PROF_FLUSHES_PER_SESSION',
    `${profFor}\n${flushProfile}\n${closeAll}\nreturn { profFor, flushProfile, closeAll };`
  );
  const m = factory(makeProfiler, flush, 3);

  return Object.assign(
    {
      env,
      profBudget: { left: 3 },
      profSession: makeProfiler(env, { target: 'session' }),
      profTargets: new Map(),
      profFlush: null,
      // closeAll 会碰到的实例状态（源码里那些字段的初值）
      streams: new Map(),
      idleTimer: null,
      authed: true,
      authPending: false,
      pendingFrames: [],
      pendingBytes: 0,
      ws: null,
    },
    m
  );
}

// recordingKV 记录所有 put，并返回可断言的快照。
function recordingKV() {
  const puts = [];
  return {
    puts,
    async put(key, value, opts) {
      puts.push({ key, value, opts });
    },
    async get() {
      return null;
    },
  };
}

test('PROFILE=1: closing a session writes a record with the collected spans', async () => {
  const kv = recordingKV();
  const s = makeSession({ PROFILE: '1', KV: kv });

  // 模拟 openExit 打过的 span：begin 返回一个收尾函数。
  const p = s.profFor('example.com:443');
  const endDirect = p.begin('exit.direct');
  endDirect();
  const endRace = p.begin('exit.race');
  endRace();
  p.count('exit.rung', 3);

  s.closeAll();
  await s.profFlush;

  const target = kv.puts.filter((x) => x.key.startsWith('profile:example.com:443:'));
  assert.equal(target.length, 1, `expected exactly 1 record for the target, got ${kv.puts.length}: ${JSON.stringify(kv.puts.map((x) => x.key))}`);

  const rec = JSON.parse(target[0].value);
  const names = rec.spans.map((x) => x.name).sort();
  assert.deepEqual(names, ['exit.direct', 'exit.race'], 'both spans must reach the record');
  assert.equal(rec.counts['exit.rung'], 3);
  assert.equal(rec.v, 1);
  // TTL 必须给：profile 是排障数据，不设过期就是永久占着 KV 的存储额度。
  assert.ok(target[0].opts?.expirationTtl > 0, 'record must expire');

  // 会话级那条（auth 用的 "session" 桶）也要落盘，否则认证耗时永远看不到。
  assert.ok(kv.puts.some((x) => x.key.startsWith('profile:session:')), 'session bucket missing');
});

test('PROFILE unset: nothing is written at all', async () => {
  const kv = recordingKV();
  const s = makeSession({ KV: kv }); // 注意：没有 PROFILE

  const p = s.profFor('example.com:443');
  p.begin('exit.direct')();
  p.count('exit.rung', 0);

  s.closeAll();
  await s.profFlush;

  assert.equal(kv.puts.length, 0, 'profile is off by default; it must not touch KV');
});

test('PROFILE=1 but no KV binding: disabled, no crash', async () => {
  const s = makeSession({ PROFILE: '1' }); // 没有 KV
  const p = s.profFor('example.com:443');
  p.begin('exit.direct')(); // 关闭态下这些必须是空操作，不能抛
  s.closeAll();
  await s.profFlush;
});

test('many targets on one session stay inside the session-wide flush budget', async () => {
  const kv = recordingKV();
  const s = makeSession({ PROFILE: '1', KV: kv });

  // 一次首屏开十几个 host 是常态。按目标各算一份配额的话，这里会写十几次 KV ——
  // 而"客户端疯狂开页面"恰恰是最不该烧配额的场景。
  for (let i = 0; i < 20; i++) {
    s.profFor(`host${i}.example:443`).begin('exit.direct')();
  }
  s.closeAll();
  await s.profFlush;

  assert.ok(
    kv.puts.length <= 3,
    `session-wide budget must cap KV writes, got ${kv.puts.length} puts for 20 targets`
  );
});

test('flushProfile clears the target cache so a long session does not grow unbounded', async () => {
  const kv = recordingKV();
  const s = makeSession({ PROFILE: '1', KV: kv });
  for (let i = 0; i < 50; i++) s.profFor(`h${i}:443`).begin('exit.direct')();
  s.closeAll();
  assert.equal(s.profTargets.size, 0, 'profTargets must be drained on flush');
  await s.profFlush;
});