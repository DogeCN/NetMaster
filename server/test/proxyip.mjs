// proxyip.mjs — HTTP CONNECT 中继出口单测。用本地 net.createServer 模拟中继
// （200 / 403 / 不回话三态），connect() 用 node:net 包装成 Workers socket 形状。
// 同时导出这套装配函数给 race.mjs 复用（那边需要同一批中继桩）。
import net from 'node:net';
import { Readable, Writable } from 'node:stream';
import { pathToFileURL } from 'node:url';
import {
	connectViaProxyIP, fallbackRelays, parseRelay, noteRelay, relayHealth, orderByHealth,
	FALLBACK_RELAY_HOSTS, RELAY_PORT, RELAY_HEALTH_TTL_MS, RELAY_TYPE_HTTP_CONNECT,
} from '../src/proxyip.js';

// ---- 本地中继桩 ----

/**
 * 起一个 HTTP CONNECT 中继。
 * mode: 'ok' | 'forbidden' | 'silent' | 'hangup'
 *   ok        200 + 回声（隧道双向可测）
 *   forbidden 403
 *   silent    收到 CONNECT 不回话（测超时）
 *   hangup    200 之后不回数据（测"连上了但目标不可达"）
 * extraBytes: 200 响应后立刻多写的字节（测 leftover 不丢）
 * splitHead: 响应头分两次写（测跨 read 边界的头解析）
 * connectDelayMs: 200 响应的延迟
 */
export async function startRelayMock(opts = {}) {
	const mode = opts.mode || 'ok';
	const extra = opts.extraBytes || '';
	const state = { requests: [], sockets: [], extraSent: Buffer.alloc(0) };
	const srv = net.createServer((sock) => {
		state.sockets.push(sock);
		let buf = '';
		let answered = false;
		const answer = () => {
			if (mode === 'silent') return;
			if (mode === 'forbidden') {
				sock.write('HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\n\r\n');
				return;
			}
			const okHead = 'HTTP/1.1 200 Connection established\r\n\r\n';
			const finish = () => {
				state.extraSent = Buffer.from(extra, 'latin1');
				if (state.extraSent.length) sock.write(state.extraSent);
			};
			if (opts.splitHead) {
				const i = okHead.lastIndexOf('\r\n\r\n');
				sock.write(okHead.slice(0, i + 2));
				setTimeout(() => { sock.write(okHead.slice(i + 2)); finish(); }, 15);
			} else {
				sock.write(okHead);
				finish();
			}
		};
		if (opts.connectDelayMs) setTimeout(answer, opts.connectDelayMs);
		sock.on('error', () => {});
		sock.on('data', (chunk) => {
			if (mode === 'ok' && answered) {
				sock.write(chunk); // 隧道字节流：回声
				return;
			}
			if (answered) return;
			buf += chunk.toString('latin1');
			const i = buf.indexOf('\r\n\r\n');
			if (i < 0) return;
			answered = true;
			state.requests.push(buf.slice(0, i));
			const rest = Buffer.from(buf.slice(i + 4), 'latin1');
			if (rest.length) sock.write(rest); // 目标首包跟在 CONNECT 请求里
			if (opts.connectDelayMs) setTimeout(answer, opts.connectDelayMs);
			else answer(); // 真实的 CONNECT 中继是收到请求才回话
		});
	});
	await new Promise((r) => srv.listen(0, '127.0.0.1', r));
	const port = srv.address().port;
	return {
		host: '127.0.0.1',
		port,
		key: `127.0.0.1:${port}`,
		state,
		close: () => {
			for (const s of state.sockets) s.destroy();
			srv.close();
		},
	};
}

/**
 * 装上假的 connect()：把 "host:port" 映射到本地端口，用 node:net 造出 Workers
 * socket 形状（opened / readable / writable / close）。每次调用记一条 entry，
 * closed 标记用来断言"输家槽位被回收了"。
 * @param {Map<string, number>} endpoints "host:port" -> 本地端口
 * @param {Array} [opened] 装配记录（key / opened / closed / at），at 用于时序断言
 */
import { setResolver } from '../src/proxyip.js';

export function installMockConnect(endpoints, opened = []) {
	setResolver(async (host) => [host]); // 恒等解析：mock 直接看到域名，不打真实 DoH
	globalThis.connect = ({ hostname, port }) => {
		const key = `${hostname}:${port}`;
		const local = endpoints.get(key);
		const entry = { key, closed: false, opened: false, at: Date.now() };
		opened.push(entry);
		const wrap = (sock) => ({
			opened: new Promise((res, rej) => {
				sock.once('connect', () => { entry.opened = true; res(); });
				sock.once('error', rej);
			}),
			readable: Readable.toWeb(sock),
			writable: Writable.toWeb(sock),
			close: () => { entry.closed = true; sock.destroy(); },
		});
		if (!local) {
			// 映射不到的端点 = 拨不通（竞速里表现为槽位立刻失败）
			return { opened: Promise.reject(new Error('connect refused (mock)')), readable: null, writable: null, close: () => { entry.closed = true; } };
		}
		return wrap(net.connect({ host: '127.0.0.1', port: local }));
	};
	return () => { delete globalThis.connect; };
}

export async function readOne(reader) {
	const { value } = await reader.read();
	return Buffer.from(value);
}

// ---- 用例 ----

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

async function run() {
	const relays = [];
	const opened = [];
	const endpoints = new Map();

	const okRelay = await startRelayMock({ mode: 'ok' });
	const forbidRelay = await startRelayMock({ mode: 'forbidden' });
	const silentRelay = await startRelayMock({ mode: 'silent' });
	const leftoverRelay = await startRelayMock({ mode: 'ok', extraBytes: 'EARLY' });
	const splitRelay = await startRelayMock({ mode: 'ok', splitHead: true });
	const hangupRelay = await startRelayMock({ mode: 'hangup' });
	const dropRelay = net.createServer((s) => s.destroy());
	await new Promise((r) => dropRelay.listen(0, '127.0.0.1', r));
	for (const r of [okRelay, forbidRelay, silentRelay, leftoverRelay, splitRelay, hangupRelay]) {
		endpoints.set(r.key, r.port);
		relays.push(r);
	}
	endpoints.set(`127.0.0.1:${dropRelay.address().port}`, dropRelay.address().port);

	const restore = installMockConnect(endpoints, opened);

	console.log('--- CONNECT 握手 ---');
	{
		const r = await connectViaProxyIP(okRelay.host, okRelay.port, 'example.com', 443);
		ok(!r.error, '200 -> tunnel established', r.error || '');
		ok(okRelay.state.requests[0] === 'CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443',
			'CONNECT request line + Host header');
		const writer = r.socket.writable.getWriter();
		await writer.write(new TextEncoder().encode('ping'));
		writer.releaseLock();
		const got = await readOne(r.socket.readable.getReader());
		ok(got.toString() === 'ping', 'tunnel carries data both ways', got.toString());
		r.socket.close();
		ok(opened[opened.length - 1].closed, 'close() reaches the underlying socket');
	}
	{
		const r = await connectViaProxyIP(leftoverRelay.host, leftoverRelay.port, 'a.example', 443);
		ok(!r.error, '200 + trailing bytes -> still established', r.error || '');
		const got = await readOne(r.socket.readable.getReader());
		ok(got.toString() === 'EARLY', 'leftover bytes are not swallowed', got.toString());
		r.socket.close();
	}
	{
		const r = await connectViaProxyIP(splitRelay.host, splitRelay.port, 'b.example', 443);
		ok(!r.error, 'response head split across reads', r.error || '');
		r.socket.close();
	}
	{
		const r = await connectViaProxyIP(forbidRelay.host, forbidRelay.port, 'c.example', 443);
		ok(!!r.error && /403/.test(r.error), '403 -> error mentions status', r.error);
		ok(opened[opened.length - 1].closed, '403 -> socket reclaimed');
	}
	{
		// 中继回了 200 却不给数据：CONNECT 层看不出来，"3 秒无首字节"的判定归
		// session 层。这里只保证不会把它误报成握手失败。
		const r = await connectViaProxyIP(hangupRelay.host, hangupRelay.port, 'g.example', 443, { timeoutMs: 300 });
		ok(!r.error, '200 with no payload -> still a success', r.error || '');
		r.socket.close();
	}
	{
		const t0 = Date.now();
		const r = await connectViaProxyIP(silentRelay.host, silentRelay.port, 'd.example', 443, { timeoutMs: 200 });
		const ms = Date.now() - t0;
		ok(!!r.error && /timeout/.test(r.error), 'silent relay -> timeout error', r.error);
		ok(ms < 1000, `timeout honoured (${ms}ms)`);
		ok(opened[opened.length - 1].closed, 'timeout -> socket reclaimed');
	}
	{
		const r = await connectViaProxyIP('127.0.0.1', dropRelay.address().port, 'e.example', 443, { timeoutMs: 500 });
		ok(!!r.error, 'relay drops the connection -> error', r.error);
	}
	{
		const r = await connectViaProxyIP('203.0.113.9', 443, 'f.example', 443, { timeoutMs: 300 });
		ok(!!r.error, 'unreachable relay -> error (no hang)', r.error);
	}

	console.log('--- 兜底列表与解析 ---');
	{
		ok(fallbackRelays().length === 9, 'fallback list has 9 relays');
		ok(fallbackRelays().every((r) => r.port === RELAY_PORT && r.host.endsWith('.CMLiussss.net')),
			'every fallback is CMLiussss on 443');
		ok(FALLBACK_RELAY_HOSTS.includes('ProxyIP.HK.CMLiussss.net'), 'HK relay present (v0.1 fallback list)');
		ok(RELAY_TYPE_HTTP_CONNECT === 'http-connect', 'egress type name');
		ok(parseRelay('1.2.3.4').port === 443, 'missing port defaults to 443');
		ok(parseRelay('1.2.3.4:8080').host === '1.2.3.4' && parseRelay('1.2.3.4:8080').port === 8080, 'explicit port');
		ok(parseRelay('ProxyIP.US.CMLiussss.net').host === 'ProxyIP.US.CMLiussss.net', 'hostname relay');
		ok(parseRelay('') === null, 'empty -> null');
	}

	console.log('--- 健康记忆 ---');
	{
		const now = 1000000;
		noteRelay('r.good', 443, true, 12, now);
		noteRelay('r.bad', 443, false, 900, now);
		ok(relayHealth('r.good', 443, now + 1000)?.ok === true, 'fresh success remembered');
		ok(relayHealth('r.good', 443, now + RELAY_HEALTH_TTL_MS + 1) === null, 'entry expires after TTL');
		// 上一条断言顺手把过期条目删了（TTL 记忆不攒垃圾），重新记一遍再看排序
		noteRelay('r.good', 443, true, 12, now);
		const ordered = orderByHealth(
			[{ host: 'r.bad', port: 443 }, { host: 'r.unknown', port: 443 }, { host: 'r.good', port: 443 }],
			now + 1000
		);
		ok(ordered[0].host === 'r.good', 'known-good first');
		ok(ordered[2].host === 'r.bad', 'known-dead last');
		ok(ordered[1].host === 'r.unknown', 'unknown keeps relative order');
	}

	restore();
	for (const r of relays) r.close();
	dropRelay.close();
	console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
	return fail === 0 ? 0 : 1;
}

// 被 race.mjs import 时不跑用例。
if (import.meta.url === pathToFileURL(process.argv[1]).href) {
	process.exit(await run());
}