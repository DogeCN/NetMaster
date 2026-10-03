// race.mjs — 6 槽位竞速单测：中继桩与假 connect() 复用 proxyip.mjs 里的那套。
// 重点是时序：交错启动、单槽超时回收、全局超时、全部失败、赢家诞生后其余槽回收。
import { startRelayMock, installMockConnect } from './proxyip.mjs';
import {
	startRace, buildCandidates, parseRelayEntries, raceConfig,
	RACE_SLOTS, RACE_SLOT_TIMEOUT_MS, RACE_GLOBAL_TIMEOUT_MS, RACE_STAGGER_MS, KV_RELAY_KEY,
} from '../src/race.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

const relays = [];
const opened = [];
const endpoints = new Map();

// 中继桩登记进全局桩表：key 就是候选里的 host:port。
async function relay(opts) {
	const r = await startRelayMock(opts);
	relays.push(r);
	endpoints.set(r.key, r.port);
	return r;
}

const cand = (r) => ({ host: r.host, port: r.port, type: "http-connect" });
const at = (key) => opened.find((e) => e.key === key).at;

const restore = installMockConnect(endpoints, opened);
const env = (extra = {}) => ({
	RACE_STAGGER_MS: 30,
	RACE_SLOT_TIMEOUT_MS: 200,
	RACE_GLOBAL_TIMEOUT_MS: 1500,
	...extra,
});

console.log('--- 配置与解析 ---');
{
	const c = raceConfig({ RACE_SLOT_TIMEOUT_MS: '900', RACE_GLOBAL_TIMEOUT_MS: -1, RACE_STAGGER_MS: 'x' });
	ok(c.slotMs === 900, 'env overrides slot timeout', String(c.slotMs));
	ok(c.globalMs === RACE_GLOBAL_TIMEOUT_MS, 'negative env falls back to default', String(c.globalMs));
	ok(c.staggerMs === RACE_STAGGER_MS, 'garbage env falls back to default', String(c.staggerMs));
	ok(c.slots === RACE_SLOTS, 'default 6 slots', String(c.slots));
	ok(RACE_SLOT_TIMEOUT_MS === 1500 && RACE_GLOBAL_TIMEOUT_MS === 3000 && RACE_STAGGER_MS === 120,
		'PRD 7.3 defaults 1.5s / 3s / 120ms');

	ok(parseRelayEntries('["a.example:443","b.example"]').length === 2, 'JSON array of strings');
	ok(parseRelayEntries('{"relays":["a.example:443"]}').length === 1, 'object with relays[]');
	ok(parseRelayEntries('{"relays":[{"host":"a.example","port":8080}]}')[0].port === 8080, 'object entries');
	ok(parseRelayEntries('{"relays":[{"host":"a.example","port":443,"type":"sni"}]}')[0].type === 'sni', 'KV entry type preserved (B1)');
	ok(parseRelayEntries('{"relays":[{"host":"a.example","port":443,"type":"bogus"}]}')[0].type === undefined, 'unknown type dropped -> dialRelay defaults');
	ok(parseRelayEntries('a.example:443\n# 注释\n\nb.example:443').length === 2, 'plain text + comments');
	ok(parseRelayEntries('a.example:443\na.example:443').length === 1, 'duplicates dropped');
	ok(parseRelayEntries('b.example')[0].port === 443, 'port defaults to 443');
	ok(parseRelayEntries('{oops').length === 0, 'broken JSON -> empty pool');
	ok(parseRelayEntries('').length === 0, 'empty KV -> empty pool');
}

console.log('--- 候选组装 ---');
{
	const kv = { KV: { get: async (k) => (k === KV_RELAY_KEY ? JSON.stringify(['k1:443', 'k2:443', 'k3:443', 'k4:443', 'k5:443']) : null) } };
	const list = await buildCandidates({ env: kv }, raceConfig(kv.env));
	ok(list.length === RACE_SLOTS, `pool fills to 6 slots (${list.length})`);
	ok(list.slice(0, 4).every((c, i) => c.host === `k${i + 1}`), 'KV top4 comes first');
	ok(list.every((c) => c.viaRouter === false), 'KV candidates are not flagged viaRouter');

	const noKv = await buildCandidates({ env: {} }, raceConfig({}));
	ok(noKv.length === RACE_SLOTS && noKv.every((c) => c.host.endsWith('.CMLiussss.net')),
		'no KV binding -> fallback list only');

	const withRouter = await buildCandidates(
		{ env: kv, routerLookup: async () => ({ host: 'r.example', port: 443, type: 'http-connect' }) },
		raceConfig(kv.env)
	);
	ok(withRouter[0].host === 'r.example' && withRouter[0].viaRouter === true, 'router hit is candidate 0 / viaRouter');
	ok(withRouter[0].type === 'http-connect', 'router hit carries its type');

	const dead = await buildCandidates(
		{ env: kv, routerLookup: async () => { throw new Error('router down'); } },
		raceConfig(kv.env)
	);
	ok(dead.length === RACE_SLOTS && dead[0].host === 'k1', 'router failure falls through to KV');

	const r = await startRace({ env: {}, candidates: [] }, { host: 'x.example', port: 443 });
	ok(!!r.error, 'no candidate -> error', JSON.stringify(r));
}

console.log('--- 竞速：选出赢家 ---');
{
	const dead1 = await relay({ mode: 'forbidden' });
	const good = await relay({ mode: 'ok' });
	const t0 = Date.now();
	const r = await startRace({ env: env(), candidates: [cand(dead1), cand(good)] }, { host: 't.example', port: 443 });
	const ms = Date.now() - t0;
	ok(!r.error, 'race resolves with a winner', r.error || '');
	ok(r.relay === good.key, 'winner is the reachable relay', r.relay);
	ok(r.type === 'http-connect', 'winner carries candidate type (B1: session learns from it)', r.type);
	ok(r.viaRouter === false, 'not via router');
	ok(ms >= 20, `slot 1 waited for its stagger slot (${ms}ms)`);
	r.socket.close();
}

console.log('--- 竞速：单槽超时后由后面的槽顶上 ---');
{
	const hang = await relay({ mode: 'silent' });
	const good = await relay({ mode: 'ok' });
	const r = await startRace({ env: env(), candidates: [cand(hang), cand(good)] }, { host: 't.example', port: 443 });
	ok(!r.error && r.relay === good.key, 'hang in slot 0 does not block slot 1', r.error || r.relay);
	await new Promise((res) => setTimeout(res, 300)); // 等槽 0 走完单槽超时
	ok(opened.filter((e) => e.key === hang.key).every((e) => e.closed), 'timed-out slot reclaimed its socket');
	r.socket.close();
}

console.log('--- 竞速：全部失败 ---');
{
	const a = await relay({ mode: 'forbidden' });
	const b = await relay({ mode: 'forbidden' });
	const t0 = Date.now();
	const r = await startRace({ env: env(), candidates: [cand(a), cand(b)] }, { host: 't.example', port: 443 });
	const ms = Date.now() - t0;
	ok(!!r.error, 'all slots failed -> error', JSON.stringify(r));
	ok(ms < RACE_GLOBAL_TIMEOUT_MS, `gives up on slot failures, not on the global timer (${ms}ms)`);
}

console.log('--- 竞速：全局超时兜底 ---');
{
	const a = await relay({ mode: 'silent' });
	const b = await relay({ mode: 'silent' });
	const t0 = Date.now();
	const r = await startRace(
		{ env: env({ RACE_SLOT_TIMEOUT_MS: 5000, RACE_GLOBAL_TIMEOUT_MS: 250 }), candidates: [cand(a), cand(b)] },
		{ host: 't.example', port: 443 }
	);
	const ms = Date.now() - t0;
	ok(!!r.error && /race timeout/.test(r.error), 'global timeout -> error', r.error);
	ok(ms < 1200, `global deadline honoured (${ms}ms)`);
}

console.log('--- 竞速：赢家诞生后其余槽回收 ---');
{
	// 两个槽都已拨号且都会成功：晚到的隧道必须被关掉，不能留在 isolate 里空转。
	const slow = await relay({ mode: 'ok', connectDelayMs: 120 });
	const quick = await relay({ mode: 'ok' });
	const r = await startRace(
		{ env: env({ RACE_STAGGER_MS: 30, RACE_SLOT_TIMEOUT_MS: 800 }), candidates: [cand(slow), cand(quick)] },
		{ host: 't.example', port: 443 }
	);
	ok(!r.error && r.relay === quick.key, 'the faster slot wins', r.error || r.relay);
	await new Promise((res) => setTimeout(res, 200)); // 让输家的隧道建起来
	ok(opened.some((e) => e.key === slow.key && e.closed), 'late winner socket closed, not left dangling');
	r.socket.close();

	// 赢家在第一个槽就出结果：还没到 stagger 时刻的槽位不该再拨号。
	const never = await relay({ mode: 'ok' });
	const r2 = await startRace(
		{ env: env({ RACE_STAGGER_MS: 200 }), candidates: [cand(slow), cand(never)] },
		{ host: 't.example', port: 443 }
	);
	ok(!r2.error, 'second scenario resolved', r2.error || '');
	await new Promise((res) => setTimeout(res, 300));
	ok(!opened.some((e) => e.key === never.key), 'pending stagger slot was cancelled, never dialed');
	r2.socket.close();
}

console.log('--- 竞速：交错时序 ---');
{
	const many = [];
	for (let i = 0; i < RACE_SLOTS; i++) many.push(await relay({ mode: 'forbidden' }));
	const stagger = 40;
	const t0 = Date.now();
	await startRace({ env: env({ RACE_STAGGER_MS: stagger }), candidates: many.map(cand) }, { host: 't.example', port: 443 });
	ok(Date.now() - t0 < RACE_GLOBAL_TIMEOUT_MS, 'whole race fits in the global budget');
	for (let i = 1; i < RACE_SLOTS; i++) {
		const gap = at(many[i].key) - at(many[0].key);
		ok(gap >= i * stagger - 15, `slot ${i} started ~${i * stagger}ms after slot 0 (gap ${gap}ms)`);
	}
}

console.log('--- KV 候选真的会被拨 ---');
{
	const kvRelay = await relay({ mode: 'ok' });
	const kv = { KV: { get: async (k) => (k === KV_RELAY_KEY ? JSON.stringify([kvRelay.key]) : null) } };
	const r = await startRace({ env: { ...env(), ...kv } }, { host: 't.example', port: 443 });
	ok(!r.error && r.relay === kvRelay.key, 'KV top relay is dialed through the real path', r.error || r.relay);
	r.socket.close();
}

restore();
for (const r of relays) r.close();
console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);