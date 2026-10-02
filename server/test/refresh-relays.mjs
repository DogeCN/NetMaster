// refresh-relays.mjs — 订阅更新器的单测。全部离线：中继桩是本地 TCP 服务器
// （复用 proxyip.mjs 的 startRelayMock），Cloudflare API 用注入的 fetch 桩，
// KV 写入用注入的 put 桩——测试绝不碰真网络、真 KV。
//
// 跑法（与 package.json 的 test 脚本一致）：
//   NODE_OPTIONS="--import ./test/shims/register.mjs" node test/refresh-relays.mjs

import { startRelayMock } from "./proxyip.mjs";
import { parseRelayEntries } from "../src/race.js";
import { RELAY_TYPE_SNI } from "../src/proxyip.js";
import {
  parseRelayList,
  fetchRelayPool,
  filterSelf,
  probeRelay,
  probeAll,
  rankRelays,
  buildTop,
  resolveNamespace,
  kvPut,
  refresh,
  KV_KEY,
  KV_TOP_N,
} from "../tools/refresh-relays.mjs";

let pass = 0, fail = 0;
const ok = (c, n, x = "") => {
  if (c) {
    pass++;
    console.log("  PASS " + n);
  } else {
    fail++;
    console.log("  FAIL " + n + " " + x);
  }
};
const eq = (a, b) => JSON.stringify(a) === JSON.stringify(b);

// ---- fetch / KV 桩 ----

// textFetch 造一个返回固定正文的 fetch；404/null 表示该源不可用。
const textFetch = (bodies) => async (url) => {
  const body = bodies[url];
  if (body === undefined) return { ok: false, status: 503, text: async () => "unreachable" };
  return { ok: true, status: 200, text: async () => body };
};

// recordingFetch 记录所有调用并返回可编程响应（用于 KV 写入断言）。
function recordingFetch(responses = {}) {
  const calls = [];
  const impl = async (url, init = {}) => {
    calls.push({ url, method: init.method || "GET", body: init.body, headers: init.headers || {} });
    for (const [match, res] of Object.entries(responses)) {
      if (url.includes(match)) return typeof res === "function" ? res() : res;
    }
    return { ok: true, status: 200, text: async () => "{}" };
  };
  impl.calls = calls;
  return impl;
}

async function run() {
  console.log("--- 源解析 ---");
  {
    const list = parseRelayList(
      ["# comment line", "", "1.2.3.4:1080  # inline note", "5.6.7.8", "1.2.3.4:1080", "relay.example:443", "  ", "garbage line:port:extra", "2001:db8::1"].join("\n")
    );
    ok(
      eq(list.map((r) => `${r.host}:${r.port}`), ["1.2.3.4:1080", "5.6.7.8:443", "relay.example:443", "2001:db8::1:443"]),
      "parseRelayList dedupes, strips comments, defaults port 443",
      JSON.stringify(list)
    );
    ok(parseRelayList("garbage line:port:extra").length === 0, "garbage line dropped instead of eating a probe slot");
    ok(parseRelayList("2001:db8::1").length === 1 && parseRelayList("2001:db8::1")[0].host === "2001:db8::1", "bare IPv6 literal is not split into host+port");
    ok(parseRelayList("").length === 0, "empty text -> no candidates");
    ok(parseRelayList(null).length === 0, "null text -> no candidates");
  }

  console.log("--- 拉源 ---");
  {
    const r = await fetchRelayPool({
      urls: ["https://src.example/list"],
      builtin: [{ host: "builtin.example", port: 443 }],
      fetchImpl: textFetch({ "https://src.example/list": "9.9.9.9:8080\n8.8.8.8:8080\n" }),
    });
    ok(r.pool.length === 3, "remote source + builtin are merged", JSON.stringify(r.pool));
    ok(r.sources.filter((s) => s.ok).length === 2, "both sources reported ok");
  }
  {
    const r = await fetchRelayPool({
      urls: ["https://src.example/list"],
      builtin: [{ host: "builtin.example", port: 443 }],
      fetchImpl: textFetch({}), // 远端源挂了
    });
    ok(r.pool.length === 1 && r.pool[0].host === "builtin.example", "remote source down -> builtin still yields a pool");
    ok(r.sources[0].ok === false && r.sources[0].error.includes("HTTP 503"), "failing source is reported, not swallowed silently");
  }
  {
    // 全挂（远端不可达 + 没有内置兜底）→ 抛错，CLI 映射成退出码 1
    let threw = null;
    try {
      await fetchRelayPool({ urls: ["https://src.example/list"], builtin: [], fetchImpl: textFetch({}) });
    } catch (e) {
      threw = e;
    }
    ok(threw !== null && /no relay candidate/.test(threw.message), "every source down -> throws (exit 1 path)");
  }

  console.log("--- 剔除回指自身 ---");
  {
    const pool = [{ host: "netmaster.example.workers.dev", port: 443 }, { host: "a.netmaster.example.workers.dev", port: 443 }, { host: "relay.example", port: 443 }];
    const kept = filterSelf(pool, "netmaster.example.workers.dev");
    ok(kept.length === 1 && kept[0].host === "relay.example", "self + subdomain dropped", JSON.stringify(kept));
    ok(filterSelf(pool, "").length === 3, "no worker host -> nothing filtered");
  }

  console.log("--- 探测（真实 TLS 握手，打公网）---");
  // probeRelay 做真 TLS 握手并校验证书：本地 TCP mock 无法模拟 CF 的证书行为，
  // 直接打公网。CF 对不存在的 SNI 回兜底证书（校验必失败）—— 这正是筛掉
  // 盲转发中继的判据；网络不可达时整段 SKIP。
  {
    const probe = await probeRelay({ host: "www.cloudflare.com", port: 443 }, { timeoutMs: 5000 });
    const reachable = probe.ok === true || !/ENOTFOUND|EAI_AGAIN/.test(probe.error || "");
    if (!probe.ok && !/certificate|ENOTFOUND|EAI_AGAIN|timeout/i.test(probe.error || "")) {
      console.log(`  SKIP: network unavailable (${probe.error})`);
    } else if (probe.ok) {
      ok(true, "real SNI relay (valid cert) -> usable", JSON.stringify(probe));
    } else {
      ok(/certificate/i.test(probe.error || ""), "bogus SNI (fallback cert) -> rejected", JSON.stringify(probe));
    }
    ok(reachable !== undefined, "probe completes");
    const rRefused = await probeRelay({ host: "127.0.0.1", port: 1 }, { timeoutMs: 1000 });
    ok(rRefused.ok === false, "closed port -> not usable", JSON.stringify(rRefused));

    console.log("--- 排序 ---");
    const fast = { host: "fast.example", port: 443, ok: true, ms: 10, error: null };
    const slow = { host: "slow.example", port: 443, ok: true, ms: 900, error: null };
    const ranked = rankRelays([slow, { host: "dead.example", port: 443, ok: false, ms: 5, error: "timeout" }, fast]);
    ok(ranked.length === 3, "rankRelays keeps failed candidates (they may recover next round)");
    ok(ranked[0].ok === undefined && ranked[0].success === 1, "usable relay ranks first");
    ok(ranked[ranked.length - 1].success === 0, "failed relay ranks last");

    console.log("--- 写入格式 ---");
    const top = buildTop(ranked, KV_TOP_N);
    ok(top.length === 2 && top.every((t) => t.host.endsWith(".example")), "buildTop keeps only usable relays", JSON.stringify(top));
    ok(eq(Object.keys(top[0]).sort(), ["host", "ms", "port", "type"]), "entry has exactly host/port/type/ms", JSON.stringify(Object.keys(top[0])));
    ok(top[0].type === RELAY_TYPE_SNI, "type is sni (public relays are SNI-routed)");
    ok(typeof top[0].ms === "number", "ms is a number");
    // 与消费端逐字段对齐：写进去的东西 race.js 必须能原样读出来
    const roundTrip = parseRelayEntries(JSON.stringify(top));
    ok(
      roundTrip.length === top.length && roundTrip.every((r, i) => r.host === top[i].host && r.port === top[i].port),
      "race.js parseRelayEntries reads back what we write",
      JSON.stringify(roundTrip)
    );

    console.log("--- last-good-wins：本轮全败不写 ---");
    const puts = [];
    const r = await refresh({
      env: { CLOUDFLARE_API_TOKEN: "t", CLOUDFLARE_ACCOUNT_ID: "a" },
      urls: [],
      builtin: [{ host: "127.0.0.1", port: 1 }],
      fetchImpl: textFetch({}),
      put: async (args) => {
        puts.push(args);
        return { bytes: 0 };
      },
      resolveNs: async () => "nsid",
      probe: async (pool, opts) => Promise.all(pool.map((relay) => probeRelay(relay, { timeoutMs: 300, target: opts.target }))),
      timeoutMs: 300,
      log: () => {},
    });
    ok(puts.length === 0, "no KV write when every probe fails", JSON.stringify(puts));
    ok(r.wrote === false && /keeping the previous/.test(r.reason), "result says last-good-wins kicked in", JSON.stringify(r.reason));
  }

  console.log("--- DRY_RUN 与 KV 写入 ---");
  {
    const good = await startRelayMock({ mode: "ok" });
    const relay = { host: "127.0.0.1", port: good.port };
    const dryPuts = [];
    const dry = await refresh({
      env: { DRY_RUN: "1" },
      urls: [],
      builtin: [relay],
      fetchImpl: textFetch({}),
      put: async (args) => dryPuts.push(args),
      probe: async (pool) => pool.map((r) => ({ host: r.host, port: r.port, ok: r.port !== 1, ms: 5, error: r.port === 1 ? "refused" : null })),
      log: () => {},
    });
    ok(dryPuts.length === 0 && dry.wrote === false, "DRY_RUN=1 prints but never writes");
    // GH workflow_dispatch 的 boolean input 渲染成 "true"，不是 "1"
    const truePuts = [];
    const dryTrue = await refresh({
      env: { DRY_RUN: "true" },
      urls: [],
      builtin: [relay],
      fetchImpl: textFetch({}),
      put: async (args) => truePuts.push(args),
      probe: async (pool) => pool.map((r) => ({ host: r.host, port: r.port, ok: r.port !== 1, ms: 5, error: r.port === 1 ? "refused" : null })),
      log: () => {},
    });
    ok(truePuts.length === 0 && dryTrue.wrote === false, 'DRY_RUN="true" (GH boolean input) also skips the write');
    const falsePuts = [];
    await refresh({
      env: { DRY_RUN: "false", CLOUDFLARE_API_TOKEN: "t", CLOUDFLARE_ACCOUNT_ID: "a", RELAY_KV_NAMESPACE_ID: "n" },
      urls: [],
      builtin: [relay],
      fetchImpl: textFetch({}),
      put: async (args) => falsePuts.push(args),
      probe: async (pool) => pool.map((r) => ({ host: r.host, port: r.port, ok: r.port !== 1, ms: 5, error: r.port === 1 ? "refused" : null })),
      log: () => {},
    });
    ok(falsePuts.length === 1, 'DRY_RUN="false" still writes');
    ok(dry.top.length === 1 && dry.top[0].host === "127.0.0.1", "dry run still reports what would be written", JSON.stringify(dry.top));

    // 真写：断言 method / URL / body 三个字段
    const fetchImpl = recordingFetch();
    const putArgs = [];
    const real = await refresh({
      env: { CLOUDFLARE_API_TOKEN: "tok", CLOUDFLARE_ACCOUNT_ID: "acct", RELAY_KV_NAMESPACE_ID: "ns123" },
      urls: [],
      builtin: [relay],
      fetchImpl,
      probe: async (pool) => pool.map((r) => ({ host: r.host, port: r.port, ok: r.port !== 1, ms: 5, error: r.port === 1 ? "refused" : null })),
      put: async (args) => {
        putArgs.push(args);
        return kvPut(args);
      },
      log: () => {},
    });
    ok(real.wrote === true && putArgs.length === 1, "a usable relay is written", JSON.stringify(real.reason));
    ok(putArgs[0].key === KV_KEY && KV_KEY === "proxyip:top", "key matches race.js KV_RELAY_KEY");
    ok(putArgs[0].ns === "ns123", "explicit RELAY_KV_NAMESPACE_ID is used without an API call");
    ok(fetchImpl.calls.length === 1 && fetchImpl.calls[0].method === "PUT", "KV write is a single PUT");
    ok(fetchImpl.calls[0].url.endsWith(`/accounts/acct/storage/kv/namespaces/ns123/values/proxyip%3Atop`), "URL has the encoded key", fetchImpl.calls[0].url);
    ok(fetchImpl.calls[0].headers.authorization === "Bearer tok", "bearer token header sent");
    ok(JSON.stringify(JSON.parse(fetchImpl.calls[0].body)) === JSON.stringify(real.top), "body is the JSON array we built", fetchImpl.calls[0].body);
  }

  console.log("--- namespace 解析 ---");
  {
    ok((await resolveNamespace({ explicit: "given", token: "t", account: "a" })) === "given", "explicit id short-circuits the API");
    const f = recordingFetch({
      "/storage/kv/namespaces": { ok: true, status: 200, text: async () => JSON.stringify({ result: [{ id: "other", title: "other" }, { id: "nsmatch", title: "netmaster" }] }) },
    });
    const ns = await resolveNamespace({ token: "t", account: "a", fetchImpl: f });
    ok(ns === "nsmatch", "title=netmaster wins over other namespaces", ns);
    ok(f.calls.length === 1 && f.calls[0].headers.authorization === "Bearer t", "namespace lookup authenticated");
  }

  console.log("--- 整轮（并发探测 + 截断）---");
  {
    const a = await startRelayMock({ mode: "ok" });
    const b = await startRelayMock({ mode: "ok", connectDelayMs: 40 });
    const logs = [];
    const r = await refresh({
      env: { DRY_RUN: "1" },
      urls: [],
      builtin: [
        { host: "127.0.0.1", port: a.port },
        { host: "127.0.0.1", port: b.port },
        { host: "127.0.0.1", port: 1 },
      ],
      fetchImpl: textFetch({}),
      probe: async (pool) => pool.map((r) => ({ host: r.host, port: r.port, ok: r.port !== 1, ms: r.port === 1 ? 300 : 5, error: r.port === 1 ? "refused" : null })),
      concurrency: 2,
      maxCandidates: 3,
      timeoutMs: 800,
      log: (m) => logs.push(m),
    });
    ok(r.top.length === 2, "both usable relays land in the top list", JSON.stringify(r.top));
    ok(r.top[0].ms <= r.top[1].ms, "faster relay ranks first", JSON.stringify(r.top.map((x) => x.ms)));
    ok(logs.some((l) => l.includes("probing 3 relays (concurrency 2")), "probe phase reports concurrency", logs.join(" | "));
    ok(logs.some((l) => l.includes("2/3 usable")), "usable count printed", logs.join(" | "));
    ok(logs.some((l) => /fail 127\.0\.0\.1:1 \d+ms \(.+\)/.test(l)), "failed probes print the reason", logs.join(" | "));

    // RELAY_MAX_CANDIDATES 环境变量能覆盖注入的默认值
    const capped = [];
    await refresh({
      env: { DRY_RUN: "1", RELAY_MAX_CANDIDATES: "1" },
      urls: [],
      builtin: [
        { host: "127.0.0.1", port: a.port },
        { host: "127.0.0.1", port: b.port },
      ],
      fetchImpl: textFetch({}),
      probe: (pool, opts) => probeAll(pool, { ...opts, timeoutMs: 800 }),
      log: (m) => capped.push(m),
    });
    ok(capped.some((l) => l.includes("probing 1 relays")), "RELAY_MAX_CANDIDATES caps the pool", capped.join(" | "));
  }
}

try {
  await run();
} finally {
  // 桩服务器随进程退出即可，这里只保证退出码语义
}

console.log(`\n${fail === 0 ? "ALL PASS" : "FAILURES"}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);