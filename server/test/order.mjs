// order.mjs — 顺序出口选择单测：成功提前、失败置后、写回只在顺序变化时发生。
// 中继桩与假 connect() 复用 proxyip.mjs 里的那套。
import { startRelayMock, installMockConnect } from './proxyip.mjs';
import {
	dialOrdered, readOrder, parseRelayEntries, orderConfig,
	ORDER_SLOTS, ORDER_SLOT_TIMEOUT_MS, KV_RELAY_KEY,
} from '../src/order.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

const relays = [];
const opened = [];
const endpoints = new Map();

async function relay(opts) {
	const r = await startRelayMock(opts);
	relays.push(r);
	endpoints.set(r.key, r.port);
	return r;
}

const cand = (r) => ({ host: r.host, port: r.port, type: "http-connect" });

const restore = installMockConnect(endpoints, opened);

console.log('--- 配置与解析 ---');
{
	const c = orderConfig({ ORDER_SLOT_TIMEOUT_MS: '900', ORDER_SLOTS: 'x' });
	ok(c.slotMs === 900, 'env overrides slot timeout', String(c.slotMs));
	ok(c.slots === ORDER_SLOTS, 'garbage slots falls back to default', String(c.slots));
	ok(ORDER_SLOT_TIMEOUT_MS === 5000, 'sequential slot timeout is 5s (cross-ocean SNI needs it)');

	ok(parseRelayEntries('["a.example:443","b.example"]').length === 2, 'JSON array of strings');
	ok(parseRelayEntries('{"relays":[{"host":"a.example","port":443,"type":"sni"}]}')[0].type === 'sni', 'type preserved');
	ok(parseRelayEntries('a.example:443\n# c\n\nb.example:443').length === 2, 'plain text + comments');
	ok(parseRelayEntries('a.example:443\na.example:443').length === 1, 'duplicates dropped');
	ok(parseRelayEntries('{oops').length === 0, 'broken JSON -> empty pool');
	ok(parseRelayEntries('').length === 0, 'empty KV -> empty pool');
}

console.log('--- readOrder ---');
{
	const kv = { KV: { get: async (k) => (k === KV_RELAY_KEY ? JSON.stringify([{ host: 'k1', port: 443 }, { host: 'k2', port: 443 }]) : null) } };
	const list = await readOrder(kv, 6);
	ok(list.length === 2 && list[0].host === 'k1', 'KV order read back in order');
	ok((await readOrder({}, 6)).length === 0, 'no KV binding -> empty');
	ok((await readOrder({ KV: { get: async () => { throw new Error('down'); } } }, 6)).length === 0, 'KV read failure -> empty');
}

console.log('--- 顺序尝试：第一个候选赢 ---');
{
	const good = await relay({ mode: 'ok' });
	const first = cand(good);
	const order = [first, { host: 'never.example', port: 443, type: 'http-connect' }];
	let reorders = 0;
	const r = await dialOrdered({ order, onReorder: () => reorders++ }, { host: 't.example', port: 443 });
	ok(!r.error && r.relay === good.key, 'first candidate wins', r.error || r.relay);
	ok(reorders === 0 && order[0] === first, 'winner already first -> no reorder, no write');
	r.socket.close();
}

console.log('--- 顺序尝试：失败置后、成功提前 ---');
{
	const dead = await relay({ mode: 'forbidden' });
	const good = await relay({ mode: 'ok' });
	const order = [cand(dead), cand(good)];
	let saved = null;
	const r = await dialOrdered({ order, onReorder: (o) => { saved = o.map((c) => c.host); } }, { host: 't.example', port: 443 });
	ok(!r.error && r.relay === good.key, 'second candidate picked up after the first failed', r.error || r.relay);
	ok(order.map((c) => c.host).join() === `${good.host},${dead.host}`, 'winner promoted to front, loser demoted to back');
	ok(saved && saved.join() === `${good.host},${dead.host}`, 'reorder callback fired exactly with the new order');
	await new Promise((res) => setTimeout(res, 100));
	ok(opened.filter((e) => e.key === dead.key).every((e) => e.closed), 'failed attempt reclaimed its socket');
	r.socket.close();
}

console.log('--- 顺序尝试：全部失败 ---');
{
	const a = await relay({ mode: 'forbidden' });
	const b = await relay({ mode: 'forbidden' });
	const order = [cand(a), cand(b)];
	const r = await dialOrdered({ order }, { host: 't.example', port: 443 });
	ok(!!r.error && /all 2/.test(r.error), 'all candidates failed -> error', JSON.stringify(r));
	ok(order.map((c) => c.host).join() === `${b.host},${a.host}`, 'losers rotated to the back in failure order');
}

console.log('--- 空候选 ---');
{
	const r = await dialOrdered({ candidates: [] }, { host: 't.example', port: 443 });
	ok(!!r.error && /no proxyip candidate/.test(r.error), 'empty pool -> explicit error', JSON.stringify(r));
}

console.log('--- 会话内存回填（getter/setter） ---');
{
	const good = await relay({ mode: 'ok' });
	const kv = { KV: { get: async (k) => (k === KV_RELAY_KEY ? JSON.stringify([{ host: good.host, port: good.port }]) : null) } };
	const sess = { relayOrder: null };
	const r = await dialOrdered(
		{
			env: kv,
			get order() { return sess.relayOrder; },
			set order(v) { sess.relayOrder = v; },
		},
		{ host: 't.example', port: 443 }
	);
	ok(!r.error && r.relay === good.key, 'KV-loaded order dialed', r.error || r.relay);
	ok(Array.isArray(sess.relayOrder) && sess.relayOrder[0].host === good.host, 'session memory backfilled from KV');
	r.socket.close();
}

restore();
for (const r of relays) r.close();
console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);
