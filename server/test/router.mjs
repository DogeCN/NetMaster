// router.mjs — Router DO 的纯逻辑与策略单测：hash 计算、TTL 判定、待写队列、
// learn/flush/forget 的 alarm 策略。没有 Workers 运行时，所以 SQLite 与 Alarm
// 用桩替身，只验"什么时候写、写几条、alarm 什么时候设/撤"。
import { createHash } from 'node:crypto';
import {
	RouterDO, RouteQueue, targetHash, routeFresh, parseLearn,
	routerName, routerShardId,
	ROUTE_TTL_MS, FLUSH_BATCH_MAX, FLUSH_DELAY_MS, ROUTER_SHARDS,
} from '../src/router.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

// ---- 桩：SQLite / Alarm ----

function fakeSql() {
	const rows = new Map();
	const calls = [];
	const exec = (sql, ...args) => {
		calls.push({ sql, args });
		if (sql.startsWith('CREATE')) return { toArray: () => [] };
		if (sql.startsWith('INSERT')) {
			rows.set(args[0], { egress_type: args[1], egress_id: args[2], updated_at: args[3] });
		} else if (sql.startsWith('SELECT')) {
			const r = rows.get(args[0]);
			return { toArray: () => (r ? [r] : []) };
		} else if (sql.startsWith('DELETE')) {
			rows.delete(args[0]);
		}
		return { toArray: () => [] };
	};
	return { exec, calls, rows };
}

function fakeState() {
	const alarms = new Set();
	return {
		alarms,
		storage: {
			sql: fakeSql(),
			setAlarm: async (t) => { alarms.add(t); },
			deleteAlarm: async () => { alarms.clear(); },
		},
	};
}

const doRequest = (path, method = 'GET', body) =>
	new Request(`https://router${path}`, {
		method,
		headers: body ? { 'content-type': 'application/json' } : {},
		body: body ? JSON.stringify(body) : undefined,
	});

console.log('--- target_hash ---');
{
	const h = await targetHash('Example.COM');
	const ref = createHash('sha256').update('example.com').digest().subarray(0, 16).toString('hex');
	ok(h === ref, 'lowercase domain, SHA-256 first 16 bytes hex', `${h} != ${ref}`);
	ok(h.length === 32 && /^[0-9a-f]{32}$/.test(h), '32 lowercase hex chars');
	ok((await targetHash('a.example')) !== (await targetHash('b.example')), 'different targets differ');
}

console.log('--- TTL 判定 ---');
{
	const now = 1_000_000_000;
	ok(routeFresh({ updated_at: now }, now), 'fresh entry');
	ok(routeFresh({ updated_at: now - ROUTE_TTL_MS + 1 }, now), 'one ms before expiry is still fresh');
	ok(!routeFresh({ updated_at: now - ROUTE_TTL_MS }, now), 'exactly at TTL counts as expired');
	ok(!routeFresh({ updated_at: now - ROUTE_TTL_MS * 3 }, now), 'long expired');
	ok(!routeFresh(null, now), 'no row');
	ok(!routeFresh({ updated_at: 'x' }, now), 'garbage updated_at');
	ok(ROUTE_TTL_MS === 3600_000, 'TTL is 1h (aligned with the cron period)');
}

console.log('--- 分片接口 ---');
{
	ok(routerName(0) === 'router:0', 'stub name');
	const shards = new Set();
	let inRange = true;
	for (let i = 0; i < 200; i++) {
		const s = routerShardId(createHash('sha256').update('t' + i).digest('hex'));
		if (!Number.isInteger(s) || s < 0 || s >= ROUTER_SHARDS) inRange = false;
		shards.add(s);
	}
	ok(inRange, 'every shard id is in [0, 16)');
	ok(shards.size > 8, `hash spreads over shards (${shards.size}/${ROUTER_SHARDS})`);
}

console.log('--- learn 请求体 ---');
{
	const good = { hash: 'a'.repeat(32), type: 'http-connect', id: 'r.example:443' };
	ok(JSON.stringify(parseLearn(good)) === JSON.stringify(good), 'valid body accepted');
	ok(parseLearn({ ...good, hash: 'A'.repeat(32) }) === null, 'uppercase hash rejected');
	ok(parseLearn({ ...good, hash: 'zz' }) === null, 'short hash rejected');
	ok(parseLearn({ ...good, type: '' }) === null, 'missing type rejected');
	ok(parseLearn({ ...good, id: '' }) === null, 'missing id rejected');
	ok(parseLearn(null) === null, 'null body rejected');
}

console.log('--- 待写队列 ---');
{
	const q = new RouteQueue();
	for (let i = 0; i < FLUSH_BATCH_MAX - 1; i++) q.push('h' + i, 'http-connect', 'r' + i);
	ok(q.size === FLUSH_BATCH_MAX - 1 && !q.full(), '49 rows -> no immediate flush');
	ok(q.push('h49', 'http-connect', 'r49') === FLUSH_BATCH_MAX && q.full(), '50th row triggers flush');
	q.push('h7', 'http-connect', 'r7-new');
	ok(q.size === FLUSH_BATCH_MAX, 'same hash overwrites instead of growing the queue');
	const batch = q.take(50);
	ok(batch.length === FLUSH_BATCH_MAX, 'take returns the whole batch');
	ok(batch.find((r) => r.hash === 'h7').id === 'r7-new', 'queue keeps the newest value per hash');
	ok(q.empty(), 'queue drained');
	ok(q.take(50).length === 0, 'take on empty queue');
}

console.log('--- learn -> 延迟 flush ---');
{
	const st = fakeState();
	const d = new RouterDO(st, {});
	const row = { hash: 'b'.repeat(32), type: 'http-connect', id: 'r.example:443' };
	const row2 = { hash: '9'.repeat(32), type: 'http-connect', id: 'r2.example:443' };
	const res = await d.fetch(doRequest('/learn', 'POST', row));
	ok(res.status === 204, 'learn answers 204', String(res.status));
	ok(st.storage.sql.calls.filter((c) => c.sql.startsWith('INSERT')).length === 0, 'learn does not write through');
	ok(st.alarms.size === 1, 'learn arms one alarm');
	const armed = [...st.alarms][0];
	ok(armed - Date.now() >= FLUSH_DELAY_MS - 50 && armed - Date.now() <= FLUSH_DELAY_MS + 200,
		`alarm is now+${FLUSH_DELAY_MS}ms`, String(armed - Date.now()));
	await d.fetch(doRequest('/learn', 'POST', row2));
	ok(st.alarms.size === 1 && [...st.alarms][0] === armed, 'alarm is armed once, not once per learn');
	await d.alarm();
	ok(st.storage.sql.calls.filter((c) => c.sql.startsWith('INSERT')).length === 2, 'alarm flushes the whole batch');
	ok(st.storage.sql.rows.get(row.hash).egress_id === 'r.example:443', 'row content matches the learn body');
	ok(st.storage.sql.rows.get(row2.hash).egress_id === 'r2.example:443', 'both rows landed');
	ok(d.alarmAt === null && d.queue.empty(), 'alarm consumed, queue empty');
	ok((await d.fetch(doRequest('/learn', 'POST', row))).status === 204, 'learn still works after a flush');
	await d.alarm();
}

console.log('--- 队列满 50 立即 flush ---');
{
	const st = fakeState();
	const d = new RouterDO(st, {});
	for (let i = 0; i < FLUSH_BATCH_MAX; i++) {
		await d.fetch(doRequest('/learn', 'POST', { hash: String(i).padStart(32, '0'), type: 'http-connect', id: `r${i}:443` }));
	}
	ok(st.storage.sql.calls.filter((c) => c.sql.startsWith('INSERT')).length === FLUSH_BATCH_MAX,
		`${FLUSH_BATCH_MAX} learns flush immediately without waiting for the alarm`);
	ok(st.alarms.size === 0, 'immediate flush on an empty queue cancels the pending alarm');
	ok(d.queue.empty(), 'queue drained by the immediate flush');
}

console.log('--- lookup ---');
{
	const st = fakeState();
	const d = new RouterDO(st, {});
	const hash = 'c'.repeat(32);
	await d.fetch(doRequest('/learn', 'POST', { hash, type: 'http-connect', id: 'r.example:443' }));
	await d.alarm();
	const hit = await d.fetch(doRequest(`/lookup?hash=${hash}`));
	ok(hit.status === 200, 'lookup hit -> 200', String(hit.status));
	const row = await hit.json();
	ok(row.id === 'r.example:443' && row.type === 'http-connect' && row.hash === hash, 'entry round-trips');

	st.storage.sql.rows.get(hash).updated_at = Date.now() - ROUTE_TTL_MS;
	ok((await d.fetch(doRequest(`/lookup?hash=${hash}`))).status === 404, 'expired entry -> 404 (falls back to race)');
	ok((await d.fetch(doRequest('/lookup?hash=nope'))).status === 404, 'malformed hash -> 404');
	ok((await d.fetch(doRequest(`/lookup?hash=${'d'.repeat(32)}`))).status === 404, 'unknown hash -> 404');
}

console.log('--- forget ---');
{
	const st = fakeState();
	const d = new RouterDO(st, {});
	const hash = 'e'.repeat(32);
	await d.fetch(doRequest('/learn', 'POST', { hash, type: 'http-connect', id: 'r.example:443' }));
	await d.alarm();
	ok(st.storage.sql.rows.has(hash), 'row is in SQLite');
	const res = await d.fetch(doRequest('/forget', 'POST', { hash }));
	ok(res.status === 204, 'forget answers 204');
	ok(!st.storage.sql.rows.has(hash), 'row deleted immediately, not queued');
	ok((await d.fetch(doRequest(`/lookup?hash=${hash}`))).status === 404, 'deleted entry -> 404');
	ok((await d.fetch(doRequest('/forget', 'POST', { hash: 'zz' }))).status === 400, 'malformed forget -> 400');
}

console.log('--- flush 失败重试 1 次后丢弃 ---');
{
	const st = fakeState();
	const d = new RouterDO(st, {});
	await d.fetch(doRequest('/learn', 'POST', { hash: 'f'.repeat(32), type: 'http-connect', id: 'r.example:443' }));
	let attempts = 0;
	d.writeBatch = () => { attempts++; throw new Error('sqlite busy'); };
	await d.alarm();
	ok(attempts === 2, `write attempted twice then dropped (${attempts})`);
	ok(d.queue.empty(), 'failed batch is discarded, not retried forever');
}

console.log('--- 未知路由 ---');
{
	const d = new RouterDO(fakeState(), {});
	ok((await d.fetch(doRequest('/whatever'))).status === 404, 'unknown path -> 404');
}

console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);