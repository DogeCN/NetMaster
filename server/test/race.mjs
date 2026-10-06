// race.mjs — 6 槽位竞速单测：中继桩与假 connect() 复用 proxyip.mjs 里的那套。
// 重点是时序：交错启动、单槽超时回收、全局超时、全部失败、赢家诞生后其余槽回收。
import { startRelayMock, installMockConnect } from './proxyip.mjs';
import {
	startRace, buildCandidates, raceConfig,
	RACE_SLOTS, RACE_SLOT_TIMEOUT_MS, RACE_GLOBAL_TIMEOUT_MS, RACE_STAGGER_MS,
} from '../src/race.js';
import { FALLBACK_RELAY_HOSTS } from '../src/proxyip.js';

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
}

console.log('--- 候选组装（硬编列表） ---');
{
	const c = raceConfig({});
	ok(c.kvTop === undefined, 'no KV knob left');
	const list = await buildCandidates({ env: {} }, c);
	ok(list.length === Math.min(RACE_SLOTS, FALLBACK_RELAY_HOSTS.length),
		`pool fills from the hardcoded list (${list.length})`);
	ok(list.every((x, i) => x.host === FALLBACK_RELAY_HOSTS[i]), 'hardcoded order preserved (measured latency asc)');
	ok(list.every((x) => x.type === 'sni'), 'all candidates are sni type');
	ok(list.every((x) => x.viaRouter === undefined), 'no router flag left');

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
	ok(r.type === 'http-connect', 'winner carries candidate type', r.type);
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

console.log('--- 硬编候选真的会被拨 ---');
{
	// 把桩中继登记成硬编列表第一名，竞速不注入 candidates 时就该拨到它。
	const hardcoded = await relay({ mode: 'ok' });
	endpoints.set(`${FALLBACK_RELAY_HOSTS[0]}:443`, endpoints.get(hardcoded.key));
	const r = await startRace({ env: env() }, { host: 't.example', port: 443 });
	ok(!r.error && r.relay === `${FALLBACK_RELAY_HOSTS[0]}:443`, 'first hardcoded relay is dialed through the real path', r.error || r.relay);
	r.socket.close();
}

restore();
for (const r of relays) r.close();
console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);