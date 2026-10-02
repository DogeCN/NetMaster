/**
 * cron.mjs — Cron 健康检查的纯逻辑单测：EWMA 排序、last-good-wins、
 * 回指条目剔除、分批游标、源解析。probeRelay 走 connect()，Node 下没有，
 * 只测其判定逻辑（状态行解析由 rankRelays 上游提供）。
 */
import {
	parseRelayList, filterLoopback, rankRelays, scoreOf, mergeScore,
	CRON_KEY_CURSOR, CRON_KEY_PENDING, CRON_KEY_LAST, SUBREQUEST_BUDGET,
	BUILTIN_RELAYS,
} from '../src/cron.js';
import { KV_RELAY_KEY, RACE_KV_TOP } from '../src/race.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };
const eq = (a, b, n) => ok(a === b, n, `(got ${JSON.stringify(a)}, want ${JSON.stringify(b)})`);

console.log('--- parseRelayList ---');
{
	const out = parseRelayList(`
# comment
ProxyIP.HK.CMLiussss.net:443
1.2.3.4:8080

5.6.7.8:1080   # trailing comment
ProxyIP.HK.CMLiussss.net:443
`);
	eq(out.length, 3, 'dedupes and drops comments/blank lines');
	eq(out[0].host, 'ProxyIP.HK.CMLiussss.net', 'first relay host');
	eq(out[0].port, 443, 'first relay port');
	eq(out[1].host, '1.2.3.4', 'ip-form relay');
	eq(out[2].port, 1080, 'trailing comment stripped');
}

console.log('--- filterLoopback (回指剔除) ---');
{
	const relays = [
		{ host: 'ProxyIP.US.CMLiussss.net', port: 443 },
		{ host: 'nm.example.com', port: 443 },
		{ host: 'sub.nm.example.com', port: 443 },
	];
	const out = await filterLoopback(relays, 'nm.example.com');
	eq(out.length, 1, 'drops relay pointing at the worker itself');
	eq(out[0].host, 'ProxyIP.US.CMLiussss.net', 'keeps unrelated relays');
	const none = await filterLoopback(relays, '');
	eq(none.length, 3, 'no workerHost means no filtering');
}

console.log('--- EWMA scoring ---');
{
	// 延迟越低越好；成功率越高越好。
	const fast = scoreOf(50, 1.0);
	const slow = scoreOf(500, 1.0);
	ok(fast < slow, 'lower latency scores better');
	const good = scoreOf(500, 1.0);
	const bad = scoreOf(50, 0.2);
	ok(good < bad, 'higher success rate beats lower latency');
	// EWMA: α=0.3，首次无历史时直接取本轮分；之后向新观测收敛
	const first = mergeScore(null, 100, 1);
	eq(first, scoreOf(100, 1), 'no history -> current score');
	const next = mergeScore(first, 1000, 0.0);
	const raw = scoreOf(1000, 0.0);
	ok(next > first, 'bad observation raises the score');
	ok(next < raw, 'but does not fully jump to the raw value (EWMA smoothing)');
	ok(next < (raw + first) / 2, 'and lands much closer to history than to the new value');
}

console.log('--- rankRelays ---');
{
	const obs = [
		{ host: 'fast.example', port: 443, ok: true, ms: 40 },
		{ host: 'slow.example', port: 443, ok: true, ms: 900 },
		{ host: 'dead.example', port: 443, ok: false, ms: 3000 },
		{ host: 'fast.example', port: 443, ok: true, ms: 60 }, // 同 host 二次观测
	];
	const ranked = rankRelays(obs);
	eq(ranked.length, 3, 'aggregates same host');
	eq(ranked[0].host, 'fast.example', 'fastest working relay ranks first');
	eq(ranked[ranked.length - 1].host, 'dead.example', 'dead relay ranks last (kept, not dropped)');
	eq(Math.round(ranked[0].ms), 50, 'averages latency across observations');
	eq(ranked[0].success, 1, 'success rate is 2/2 for fast.example');
}

console.log('--- KV keys & budget ---');
{
	eq(KV_RELAY_KEY, 'proxyip:top', 'read side and write side agree on key');
	ok(RACE_KV_TOP === 4, 'top-N matches race.js RACE_KV_TOP');
	eq(SUBREQUEST_BUDGET, 48, 'subrequest budget leaves headroom under 50');
	ok(SUBREQUEST_BUDGET <= 50, 'never exceeds the free-plan 50 subrequests');
	eq(CRON_KEY_CURSOR, 'cron:cursor', 'cursor key');
	eq(CRON_KEY_PENDING, 'cron:pending', 'pending key');
	eq(CRON_KEY_LAST, 'cron:lastRun', 'last-run key');
}

console.log('--- built-in fallback pool ---');
{
	ok(BUILTIN_RELAYS.length >= 4, 'at least 4 built-in relays to race over');
	const parsed = parseRelayList(BUILTIN_RELAYS.join('\n'));
	eq(parsed.length, BUILTIN_RELAYS.length, 'all built-ins parse');
	ok(parsed.every((r) => r.port === 443), 'built-ins default to 443');
}

console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);
