/**
 * Unit tests for the internal protocol: session-open header and mux frames.
 * Run: node test/protocol.mjs
 */
import { parseSessionOpen, buildSessionOpen, buildMuxFrame, parseMuxFrame } from '../src/protocol.js';
import { bytesToHex } from '../src/crypto.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };
const eq = (a, b, n) => ok(a === b, n, `(got ${JSON.stringify(a)}, want ${JSON.stringify(b)})`);
const eqArr = (a, b, n) => { const av = Array.from(a || []), bv = Array.from(b || []); ok(av.length === bv.length && av.every((v, i) => v === bv[i]), n, `(got ${JSON.stringify(av)}, want ${JSON.stringify(bv)})`); };

console.log('--- session-open header ---');
{
	const req = buildSessionOpen({ host: 'example.com', port: 443, payload: Uint8Array.from([1, 2, 3]) });
	const r = parseSessionOpen(req);
	eq(r.status, 'ok', 'parses session open');
	eq(r.result.host, 'example.com', 'host');
	eq(r.result.port, 443, 'port');
	eqArr(r.result.payload, [1, 2, 3], 'payload');
}
{
	const req = buildSessionOpen({ host: '93.184.216.34', port: 8443 });
	const r = parseSessionOpen(req);
	eq(r.status, 'ok', 'parses IPv4');
	eq(r.result.host, '93.184.216.34', 'IPv4 host');
	eq(r.result.port, 8443, 'port 8443');
}
{
	const req = buildSessionOpen({ host: '2606:4700:4700::1111', port: 443 });
	const r = parseSessionOpen(req);
	eq(r.status, 'ok', 'parses IPv6');
	eq(r.result.host, '2606:4700:4700:0:0:0:0:1111', 'IPv6 host (expanded form)');
}
{
	// 无版本、无 UUID、无命令、无 addon —— 一个字节都不给协议之外的语义。
	const req = buildSessionOpen({ host: 'a.com', port: 1 });
	eq(req[0], 0, 'first byte is the port high byte (no version byte)');
	eq(req.length, 3 + 1 + 5, 'header is exactly port+atyp+addr');
}
{
	eq(parseSessionOpen(buildSessionOpen({ host: 'a.com', port: 1 })).status, 'ok', 'minimal header parses');
	eq(parseSessionOpen(new Uint8Array([0, 1])).status, 'need_more', 'need_more when short');
	eq(parseSessionOpen(new Uint8Array([0, 80, 9, 1, 2, 3, 4])).status, 'invalid', 'rejects bad address type');
	eq(parseSessionOpen(buildSessionOpen({ host: 'a.com', port: 0 })).status, 'invalid', 'rejects port 0');
}

console.log('--- mux frames ---');
{
	const SID = Uint8Array.from([0xaa, 0xbb, 0xcc]);
	const payload = Uint8Array.from([9, 8, 7]);
	const f = buildMuxFrame(SID, payload);
	eq(f[0], 3, 'idLen byte');
	eqArr(f.slice(1, 4), [0xaa, 0xbb, 0xcc], 'frame id');
	eqArr(f.slice(4), [9, 8, 7], 'frame payload');
	const back = parseMuxFrame(f);
	eq(bytesToHex(back.sessionId), 'aabbcc', 'parseMuxFrame id');
	eqArr(back.payload, [9, 8, 7], 'parseMuxFrame payload');
	eq(parseMuxFrame(f.slice(0, 1)), null, 'incomplete frame -> null');
	eq(parseMuxFrame(Uint8Array.from([3, 0xaa])), null, 'truncated id -> null');
	eq(parseMuxFrame(Uint8Array.from([0, 1, 2])), null, 'idLen 0 is not a session frame (control marker)');
}

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
