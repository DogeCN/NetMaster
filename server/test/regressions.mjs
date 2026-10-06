// 这些用例针对一次审查里查出的缺陷，全部遵守同一条规矩：
// **回退实现，本文件必须变红**。绿了不算数。
//
// 为什么它们能存在，而 CRITICAL-1 那类缺陷当初抓不住：
// 审查指出 openExit 的块级作用域 bug 时，出口层测试测了出口失败返回 error、
// `integration.mjs` 测了客户端收到 0x03，两条都绿 —— 但 integration 连的是
// devserver 这个**替身**，真实的 SessionDO.openExit 零覆盖，而
// `node --check` 只查语法、查不出运行期未定义标识符。
//
// 于是这里换了个打法：不走端到端，而是把被测的那段**源码文本本身**抠出来跑。
// 作用域 bug、别名 import 这类问题正是文本级的，文本级测试恰好能看见。

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';

const here = dirname(fileURLToPath(import.meta.url));
const srcDir = join(here, '..', 'src');
const read = (f) => readFileSync(join(srcDir, f), 'utf8');

// ---------------- CRITICAL-1：openExit 的 direct 作用域 ----------------

// 把 openExit 的方法体抠出来、配上它依赖的几个桩，真的调一次。
//
// 这么写看着别扭，但它是**唯一**能在不引入打包器的前提下测到 SessionDO 内部的办法：
// session.js 用约 15 个来自其他模块的符号，靠 build.mjs 拼接共享作用域，
// 所以 `import('./src/session.js')` 直接报 "Only URLs with a scheme in ..."。
function extractOpenExit(sessionSrc) {
  const start = sessionSrc.indexOf('  async openExit(atyp, host, port) {');
  assert.ok(start > 0, 'openExit not found in session.js');
  // 花括号配平，截到方法结束为止。
  let depth = 0;
  let i = sessionSrc.indexOf('{', start);
  const from = i;
  for (; i < sessionSrc.length; i++) {
    const ch = sessionSrc[i];
    if (ch === '{') depth++;
    else if (ch === '}') {
      depth--;
      if (depth === 0) {
        i++;
        break;
      }
    }
  }
  return sessionSrc.slice(start, i);
}

function makeOpenExit(overrides = {}) {
  const src = extractOpenExit(read('session.js'));
  const body = src.replace('async openExit(atyp, host, port) {', 'async function openExit(atyp, host, port) {');
  const factory = new Function(
    'targetHash', 'directConnect', 'dialRelay', 'dialOrdered', 'parseRelay',
    'RELAY_TYPE_HTTP_CONNECT', 'RELAY_TYPE_SNI', 'console',
    `${body}\nreturn openExit;`
  );
  const cfg = {
    directFails: true,
    raceFails: true,
    ...overrides,
  };
  const ctx = {
    env: {},
    directFailed: new Set(),
    egress: new Map(),
    log() {},
    rememberEgress() {},
    // openExit 现在会取目标采集器来给四级阶梯打 span。这里给一个**结构相同**
    // 的空实现（profile.js 里 enabled=false 时就是这个样子：不花任何代价）。
    //
    // 为什么要照着 profile.js 的关闭态形状写、而不是直接给 makeProfiler：
    // 这个 ctx 是手工拼的，它替的是"一个没开 PROFILE 的 SessionDO"。给了真
    // makeProfiler 就得连 env.PROFILE / KV 一起编进来，那是把单元测试变成集成测试；
    // 而给一个空壳又会在 profile.js 改了形状之后悄悄失配。所以两边都保持最小。
    profFor: () => ({
      begin: () => () => {},
      mark() {},
      count() {},
    }),
    ...overrides.self,
  };
  const targetHash = async () => 'h';
  const directConnect = async () => (cfg.directFails ? { error: 'blocked' } : { socket: {} });
  const dialOrdered = async () => (cfg.raceFails ? { error: 'all 6 proxyip exits failed' } : { socket: {}, relay: 'r:1', type: 'sni' });
  const fn = factory(targetHash, directConnect, async () => ({ error: 'x' }), dialOrdered,
    () => ({ host: 'r', port: 1 }), 'http-connect', 'sni', console).bind(ctx);
  // 返回 ctx 是为了让用例能直接摆布实例状态（比如"本会话已判死直连"）。
  // bind() 出来的是函数，属性不在上面。
  return Object.assign(fn, { ctx });
}

// TestOpenExitReturnsAnErrorWhenEverythingFails 是 CRITICAL-1 的钉子。
//
// 上一版把 `const direct` 写在 if 块里，却在下面的竞速失败分支引用它 —— 块级作用域，
// 必然 ReferenceError。而它命中的正是最常见的故障态：直连失败且竞速 6 槽全灭。
// 后果不是"抛个异常"，而是 openStream 还没来得及发 STATUS_NOEXIT 就 reject，
// 客户端拿不到任何响应帧，只能干等 20s 超时；认证期间排队的开帧也一起丢掉。
//
// 回退到块内 const 时本用例报 ReferenceError。
test('openExit reports an error when direct and every ordered relay fail', async () => {
  const openExit = makeOpenExit();
  const res = await openExit('example.com', 443);
  assert.equal(res.error, 'blocked; all 6 proxyip exits failed',
    `expected a normal error result, got ${JSON.stringify(res)}`);
  assert.ok(!res.socket);
});

// TestOpenExitWhenDirectAlreadyKnownBad 覆盖 direct 为 null 的那条支路。
//
// 本会话早前已把直连判死（directFailed 命中）时，代码根本不拨 direct。
// 修好作用域还不够：直接读 direct.error 会变成 null 上的属性访问，换一种崩法。
test('openExit survives a session where direct was already proven bad', async () => {
  const openExit = makeOpenExit();
  openExit.ctx.directFailed.add('h'); // 本会话早前已判死直连 ⇒ direct 为 null
  const res = await openExit('example.com', 443);
  assert.ok(res.error, 'must still produce an error result, not throw');
  assert.match(res.error, /already failed earlier this session/);
});

// TestOpenExitSucceedsViaDirect 是对照组：直连通就该直接返回。
test('openExit returns the direct socket without walking the relay list', async () => {
  const openExit = makeOpenExit({ directFails: false });
  const res = await openExit('example.com', 443);
  assert.ok(res.socket, 'a working direct exit must be used as-is');
  assert.ok(!res.error);
});

// ---------------- CRITICAL-2：打包器不能静默丢模块 ----------------

// TestBundlerRejectsRenamedImports 守的是这轮改动自己踩到的坑。
//
// `import { flush as flushProfile }` 在源码里读着完全正常，而拼接式打包只是把 import
// 语句整行删掉：flushProfile 从未进入 bundle，调用点是运行期 ReferenceError，
// 而 node --check 照样通过。当时 session.js 里真的就写着这么一行。
test('build refuses an import alias it cannot honour', () => {
  const backup = read('session.js');
  const broken = backup.replace(
    "import { makeProfiler, flush } from './profile.js';",
    "import { makeProfiler, flush as flushProfile } from './profile.js';"
  );
  assert.notEqual(broken, backup, 'fixture did not apply — update this test with the import');
  write('session.js', broken);
  try {
    buildFails(['build.mjs'], /renamed import/, 'a renamed import');
  } finally {
    write('session.js', backup);
  }
});

test('build refuses an import of a module that does not exist', () => {
  const backup = read('session.js');
  write('session.js', backup.replace("from './profile.js'", "from './nope.js'"));
  try {
    buildFails(['build.mjs'], /not in src/, 'an import of a missing module');
  } finally {
    write('session.js', backup);
  }
});

// TestBuildPullsInEveryModuleThatExists 是这次修复的核心承诺：
// src/ 里多一个文件，构建就多一个模块。上一版那句手写数组做不到。
test('build picks up a module nobody listed anywhere', () => {
  const stray = join(srcDir, '__stray_probe.js');
  writeFileSync(stray, 'export function strayProbe() { return 1; }\n');
  try {
    const out = buildOutput(['build.mjs']);
    const m = out.match(/from (\d+) modules/);
    assert.ok(m, `build output has no module count:\n${out}`);
    const n = Number(m[1]);
    const files = readdirSync(srcDir).filter((f) => f.endsWith('.js')).length;
    assert.equal(n, files,
      `build bundled ${n} modules but src/ holds ${files} — a file was silently dropped`);
  } finally {
    rmSync(stray, { force: true });
    run(['build.mjs']);
  }
});

// ---------------- HIGH：ATYP_DOMAIN 的字符集 ----------------

// 用真实的常量，别写死数字 —— 第一版把 ATYP_DOMAIN 写成 0x03，那是 IPV6，
// 于是"恶意主机名"被当成 IPv6 解析，测试红得莫名其妙而实现其实是对的。
async function domainFrame(host) {
  const { ATYP_DOMAIN } = await import('../src/protocol.js');
  return Buffer.concat([
    Buffer.from([ATYP_DOMAIN]),
    Buffer.from([host.length]),
    Buffer.from(host, 'latin1'),
    Buffer.from([0x01, 0xbb]), // port 443
  ]);
}

test('hostname with CRLF is rejected instead of split-injecting a relay request', async () => {
  const { parseAddr } = await import('../src/protocol.js');
  const evil = 'a.example\r\nx-injected: pwned\r\n\r\nget /evil:443 HTTP/1.1';
  const got = parseAddr(new Uint8Array(await domainFrame(evil)), 0);
  assert.equal(got, null,
    'a hostname carrying CR/LF must be refused — it is spliced verbatim into a ' +
    'bare HTTP request to third-party relays');
});

test('ordinary hostnames still parse', async () => {
  const { parseAddr } = await import('../src/protocol.js');
  for (const host of ['example.com', 'a-b.example.co.uk', 'xn--fiqs8s.test', 'my_host.internal']) {
    const got = parseAddr(new Uint8Array(await domainFrame(host)), 0);
    assert.ok(got, `${host} should parse`);
    assert.equal(got.addr, host);
    assert.equal(got.port, 443);
  }
});

// ---- 小工具 ----
import { writeFileSync, rmSync, readdirSync } from 'node:fs';

function write(file, content) {
  writeFileSync(join(srcDir, file), content, 'utf8');
}

function run(args) {
  try {
    const stdout = execFileSync(process.execPath, args, {
      cwd: join(here, '..'),
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { status: 0, stdout, stderr: '' };
  } catch (e) {
    return { status: e.status, stdout: e.stdout || '', stderr: e.stderr || '' };
  }
}

// build 的诊断走 console.error（stderr），所以断言必须看两处的合集。
// 只看 stdout 会让"构建失败了但理由没匹配上"显示成"构建接受了它"。
function buildOutput(args) {
  const r = run(args);
  return `${r.stdout}\n${r.stderr}`;
}

function buildFails(args, pattern, what) {
  const r = run(args);
  const out = `${r.stdout}\n${r.stderr}`;
  if (r.status === 0) {
    assert.fail(`build accepted ${what} (exit 0):\n${out}`);
  }
  assert.match(out, pattern, `build rejected it, but not for the expected reason:\n${out}`);
}