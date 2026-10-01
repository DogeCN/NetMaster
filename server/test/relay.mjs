/**
 * Outbound dialing rules.
 *
 * Cloudflare blocks Worker outbound TCP to Cloudflare's own IP ranges, so a
 * Cloudflare-fronted site cannot be reached directly. Direct is still tried
 * FIRST — a blocked target is refused in about ten milliseconds, so the relay
 * fallback costs almost nothing when unused, and non-Cloudflare targets never
 * pay for the extra hop.
 */
import { Forwarder } from '../src/forward.js';
import { parseRelayList } from '../src/forward.js';
import { authBytes } from '../src/crypto.js';
import { buildSessionOpen, buildMuxFrame } from '../src/protocol.js';

const PASSWORD = 'relay-password';
const AUTH = authBytes(PASSWORD);

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

const RELAYS = ['relay1.test', 'relay2.test'];

/** Records every connect() attempt, always failing so we can inspect them. */
function probe(targetPort, host = 'example.com', relays = RELAYS) {
	const calls = [];
	const request = {
		fetcher: {
			connect(opts) { calls.push(opts); throw new Error('probe-stop'); },
		},
	};
	const fakeWs = { binaryType: '', addEventListener() {}, send() {}, close() {} };
	const fwd = new Forwarder(request, fakeWs, AUTH, { log: () => {}, relays });
	const sid = Uint8Array.from([1, 2, 3, 4]);
	return fwd.onMessage(AUTH).then(() => fwd.onMessage(buildMuxFrame(sid, buildSessionOpen({
		host, port: targetPort,
	})))).then(() => calls);
}

console.log('--- relay source parsing ---');
{
	const list = parseRelayList('1.2.3.4\n5.6.7.8:443#JP\n9.9.9.9:8443\nhost.example.com\n<html>bad</html>\n\n1.2.3.4');
	ok(list.includes('1.2.3.4.sslip.io'), 'bare IPv4 wrapped in sslip.io', JSON.stringify(list));
	ok(list.includes('5.6.7.8.sslip.io'), 'IPv4 with 443 kept', JSON.stringify(list));
	ok(!list.some((r) => r.startsWith('9.9.9.9')), 'non-443 port dropped', JSON.stringify(list));
	ok(list.includes('host.example.com'), 'hostname passed through', JSON.stringify(list));
	ok(!list.some((r) => r.includes('html')), 'HTML noise dropped', JSON.stringify(list));
	ok(list.filter((r) => r === '1.2.3.4.sslip.io').length === 1, 'deduped', JSON.stringify(list));
}

console.log('--- port 443 tries direct first, then each relay ---');
{
	const calls = await probe(443);
	ok(calls.length === 1 + RELAYS.length, 'tries direct plus every relay', `n=${calls.length}`);
	ok(calls[0].hostname === 'example.com' && calls[0].port === 443, 'direct attempt comes first', JSON.stringify(calls[0]));
	const relays = calls.slice(1);
	ok(relays.every((c) => RELAYS.includes(c.hostname)), 'then the configured relays', JSON.stringify(relays));
	ok(relays.every((c) => c.port === 443), 'relays are dialed on 443', JSON.stringify(relays.map((c) => c.port)));
}

console.log('--- relays are only used for 443 ---');
for (const port of [80, 22, 3306, 8443]) {
	const calls = await probe(port);
	ok(calls.length === 1 && calls[0].port === port, `port ${port}: direct only, no relay`, JSON.stringify(calls));
}

console.log('--- relays: [] disables the fallback ---');
{
	const calls = await probe(443, 'example.com', []);
	ok(calls.length === 1, 'only the direct attempt', JSON.stringify(calls));
}

console.log('--- a zero-byte relay stream is forgotten so the next session re-probes ---')
{
	const calls = [];
	const request = {
		fetcher: {
			connect(opts) {
				calls.push(opts);
				if (opts.hostname === 'goodrelay.test') {
					// Opens fine but delivers nothing: the "half-dead relay" shape.
					return {
						opened: Promise.resolve(),
						readable: new ReadableStream({ start(c) { c.close(); } }),
						writable: new WritableStream(),
						close() {},
					};
				}
				throw new Error('blocked');
			},
		},
	};
	const fakeWs = { binaryType: '', addEventListener() {}, send() {}, close() {} };
	const sid = Uint8Array.from([1, 1, 2, 2]);
	{
		const fwd = new Forwarder(request, fakeWs, AUTH, { log: () => {}, relays: ['badrelay.test', 'goodrelay.test'] });
		await fwd.onMessage(AUTH);
		await fwd.onMessage(buildMuxFrame(sid, buildSessionOpen({ host: 'forget.test', port: 443 })));
		await new Promise((r) => setTimeout(r, 50));
		ok(calls.some((c) => c.hostname === 'goodrelay.test'), 'the session went through goodrelay');
	}
	// Same isolate, same host: the binding to goodrelay must have been forgotten,
	// so the next session re-probes and can pick differently.
	{
		const calls2 = [];
		const request2 = {
			fetcher: {
				connect(opts) {
					calls2.push(opts);
					if (opts.hostname === 'badrelay.test') {
						return {
							opened: Promise.resolve(),
							readable: new ReadableStream({ start(c) { c.enqueue(Buffer.from('HTTP/1.1 200 OK\r\n\r\n')); c.close(); } }),
							writable: new WritableStream(),
							close() {},
						};
					}
					throw new Error('blocked');
				},
			},
		};
		const fwd2 = new Forwarder(request2, fakeWs, AUTH, { log: () => {}, relays: ['badrelay.test', 'goodrelay.test'] });
		await fwd2.onMessage(AUTH);
		await fwd2.onMessage(buildMuxFrame(sid, buildSessionOpen({ host: 'forget.test', port: 443 })));
		await new Promise((r) => setTimeout(r, 50));
		ok(calls2[0] !== undefined && calls2[0].hostname === 'forget.test', 'direct is still first');
		// 遗忘生效的证明：badrelay 被重新尝试 —— 若仍钉死 goodrelay，
		// shuffled 顺序再怎么变也不会碰它。
		ok(calls2.some((c) => c.hostname === 'badrelay.test'),
			'forgotten binding re-probes instead of pinning goodrelay', JSON.stringify(calls2.map((c) => c.hostname)));
	}
}

console.log('--- a fully blocked target reports the reason ---');
{
	const request = {
		fetcher: { connect() { throw new Error('proxy request failed, cannot connect to the specified address'); } },
	};
	const sent = [];
	const fakeWs = { binaryType: '', addEventListener() {}, send(b) { sent.push(Buffer.from(b)); }, close() {} };
	const fwd = new Forwarder(request, fakeWs, AUTH, { log: () => {}, relays: RELAYS });
	const sid = Uint8Array.from([9, 9, 9, 9]);
	await fwd.onMessage(AUTH);
	await fwd.onMessage(buildMuxFrame(sid, buildSessionOpen({
		host: 'example.com', port: 443,
	})));
	ok(sent.length > 0, 'client got a control frame');
	if (sent.length) {
		const f = sent[0];
		ok(f[0] === 0, 'control frame marker');
		const msg = f.slice(2 + f[1]).toString();
		ok(msg.includes('example.com:443'), 'names the target', msg);
		ok(/proxy request failed/.test(msg), 'relays the runtime reason', msg);
	} else {
		fail++;
	}
}

console.log('--- relay affinity: a host remembers the relay that worked ---');
{
	// First contact: probing is unavailable (no fetcher.fetch), direct fails;
	// relayA succeeds.
	const calls = [];
	let succeeded = null;
	const request = {
		fetcher: {
			connect(opts) {
				calls.push(opts);
				if (opts.hostname === 'relayA.test') {
					succeeded = opts.hostname;
					// Valid success shape: must actually deliver bytes, otherwise
					// the zero-byte forget treats it as a half-dead relay.
					return {
						opened: Promise.resolve(),
						readable: new ReadableStream({ start(c) { c.enqueue(Buffer.from('HTTP/1.1 200 OK\r\n\r\n')); c.close(); } }),
						writable: new WritableStream(),
						close() {},
					};
				}
				throw new Error('blocked');
			},
		},
	};
	const fakeWs = { binaryType: '', addEventListener() {}, send() {}, close() {} };
	const fwd = new Forwarder(request, fakeWs, AUTH, { log: () => {}, relays: ['relayA.test', 'relayB.test', 'relayC.test'] });
	const sid = Uint8Array.from([7, 7, 7, 7]);
	await fwd.onMessage(AUTH);
	await fwd.onMessage(buildMuxFrame(sid, buildSessionOpen({
		host: 'affinity.test', port: 443,
	})));
	await new Promise((r) => setTimeout(r, 50));
	ok(succeeded === 'relayA.test', 'relayA was the one that connected');

	// Second contact for the SAME host: relayA must now be tried before the rest.
	const calls2 = [];
	const request2 = {
		fetcher: { connect(opts) { calls2.push(opts); throw new Error('x'); } },
	};
	const fwd2 = new Forwarder(request2, fakeWs, AUTH, { log: () => {}, relays: ['relayA.test', 'relayB.test', 'relayC.test'] });
	const sid2 = Uint8Array.from([8, 8, 8, 8]);
	await fwd2.onMessage(AUTH);
	await fwd2.onMessage(buildMuxFrame(sid2, buildSessionOpen({
		host: 'affinity.test', port: 443,
	})));
	ok(calls2[0].hostname === 'affinity.test', 'direct is still first');
	ok(calls2[1].hostname === 'relayA.test', 'the remembered relay goes first', JSON.stringify(calls2.map((c) => c.hostname)));

	// A DIFFERENT host has no affinity yet, so it must not be pinned to relayA.
	const calls3 = [];
	const request3 = {
		fetcher: { connect(opts) { calls3.push(opts); throw new Error('x'); } },
	};
	const fwd3 = new Forwarder(request3, fakeWs, AUTH, { log: () => {}, relays: ['relayA.test', 'relayB.test', 'relayC.test'] });
	const sid3 = Uint8Array.from([6, 6, 6, 6]);
	await fwd3.onMessage(AUTH);
	await fwd3.onMessage(buildMuxFrame(sid3, buildSessionOpen({
		host: 'other.test', port: 443,
	})));
	ok(calls3.length === 1 + 3, 'other host still tries all relays', `n=${calls3.length}`);
}

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
