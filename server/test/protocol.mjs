// protocol.mjs — v2 帧编解码单测：首帧/开帧/数据/控制/响应全往返，
// IPv6 字面量解析（含 :: 缩写与内嵌 IPv4），流 ID 边界。
import {
	encodeAddr, parseAddr, encodeFirstFrame, parseFirstFrame,
	encodeOpenFrame, parseOpenFrame, encodeDataFrame, encodeResponse,
	encodeCloseControl, isControl, parseCloseControl, streamIdFromBytes,
	parseIPv6, formatIPv6, validStreamId,
	MAX_PAYLOAD, STATUS_OK, STATUS_BAD, STATUS_FORBIDDEN, STATUS_NOEXIT,
	ATYP_IPV4, ATYP_DOMAIN, ATYP_IPV6, STREAM_ID_MAX,
} from '../src/protocol.js';
import { authCode } from '../src/crypto.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };
const eq = (a, b, n) => ok(a === b, n, `(got ${typeof a === 'bigint' ? a.toString() : JSON.stringify(a)}, want ${typeof b === 'bigint' ? b.toString() : JSON.stringify(b)})`);

console.log('--- encodeAddr / parseAddr ---');
{
	const b = encodeAddr('example.com', 443);
	eq(b[0], ATYP_DOMAIN, 'domain atyp');
	eq(b[1], 11, 'domain length byte');
	const p = parseAddr(b, 0);
	eq(p.addr, 'example.com', 'domain roundtrip');
	eq(p.port, 443, 'port');
	eq(p.len, 1 + 1 + 11 + 2, 'consumed length includes port');
}
{
	const b = encodeAddr('1.2.3.4', 65535);
	eq(b[0], ATYP_IPV4, 'ipv4 atyp');
	const p = parseAddr(b, 0);
	eq(p.addr, '1.2.3.4', 'ipv4 roundtrip');
	eq(p.port, 65535, 'port 65535');
}
{
	const b = encodeAddr('2001:4860:4860::8888', 443);
	eq(b[0], ATYP_IPV6, 'ipv6 atyp');
	eq(b.length, 1 + 16 + 2, 'ipv6 addr is 16 bytes');
	const p = parseAddr(b, 0);
	eq(p.addr, '2001:4860:4860:0000:0000:0000:0000:8888', 'ipv6 roundtrip (expanded)');
}
ok(encodeAddr('', 80) === null, 'empty domain rejected');
ok(encodeAddr('a'.repeat(256), 80) === null, '256-byte domain rejected');
ok(encodeAddr('1.2.3.256', 80) === null, 'invalid ipv4 rejected');

console.log('--- first frame ---');
{
	const now = Math.floor(Date.now() / 1000);
	const auth = await authCode('pw', encodeFirstFrame(new Uint8Array(16), now, 1, 'example.com', 443).slice(16));
	const f = encodeFirstFrame(auth, now, 1, 'example.com', 443);
	const p = parseFirstFrame(f);
	eq(p.streamId, 1, 'stream id');
	eq(p.host, 'example.com', 'host');
	eq(p.port, 443, 'port');
	eq(p.ts, BigInt(now), 'ts');
	eq(f.length, 16 + 8 + 4 + 1 + 1 + 11 + 2, 'first frame total length');
	const expect = await authCode('pw', p.signed);
	ok(Buffer.from(expect).equals(Buffer.from(auth)), 'auth verifies over signed region');
}
ok(parseFirstFrame(new Uint8Array(20)) === null, 'short first frame rejected');

console.log('--- open / data / control / response ---');
{
	const o = encodeOpenFrame(7, '10.0.0.1', 80);
	const p = parseOpenFrame(o);
	eq(p.streamId, 7, 'open stream id');
	eq(p.host, '10.0.0.1', 'open host');
	eq(p.port, 80, 'open port');
}
{
	const d = encodeDataFrame(9, Uint8Array.from([1, 2, 3]));
	eq(streamIdFromBytes(d), 9, 'data stream id');
	eq(d.length, 7, 'data frame length');
	eq(d[4], 1, 'payload byte');
}
{
	const r = encodeResponse(0xfffffffe, STATUS_FORBIDDEN);
	eq(r.length, 5, 'response is 5 bytes');
	eq(r[3], 0xfe, 'stream id low byte');
	eq(r[4], STATUS_FORBIDDEN, 'status byte');
}
{
	const c = encodeCloseControl(42);
	ok(isControl(c), 'close control prefix 0x00000000');
	eq(parseCloseControl(c), 42, 'close stream id');
	eq(c.length, 9, 'close control is 9 bytes');
	ok(!isControl(encodeOpenFrame(0x00000001, 'a.com', 80)), 'nonzero id is not control');
	ok(parseCloseControl(Uint8Array.of(0, 0, 0, 0, 7, 0, 0, 0, 1)) === null, 'unknown ctrl type rejected');
}

console.log('--- stream id rules ---');
{
	ok(validStreamId(1) && validStreamId(STREAM_ID_MAX), 'valid range');
	ok(!validStreamId(0), 'zero rejected');
	ok(!validStreamId(0xffffffff), '0xFFFFFFFF rejected');
	ok(!validStreamId(STREAM_ID_MAX + 1), 'above max rejected');
}

console.log('--- IPv6 literal parsing ---');
{
	const cases = [
		['::', '0000:0000:0000:0000:0000:0000:0000:0000'],
		['::1', '0000:0000:0000:0000:0000:0000:0000:0001'],
		['64:ff9b::808:808', '0064:ff9b:0000:0000:0000:0000:0808:0808'],
		['2001:67c:2960:6464::808:808', '2001:067c:2960:6464:0000:0000:0808:0808'],
		['2606:4700:4700::1111', '2606:4700:4700:0000:0000:0000:0000:1111'],
		['::ffff:1.2.3.4', '0000:0000:0000:0000:0000:ffff:0102:0304'],
	];
	for (const [input, want] of cases) {
		const b = parseIPv6(input);
		ok(b !== null && formatIPv6(b) === want, `parse ${input}`, `(got ${b && formatIPv6(b)})`);
	}
	ok(parseIPv6('1:2:3:4:5:6:7:8:9') === null, '9 groups rejected');
	ok(parseIPv6('1::2::3') === null, 'double :: rejected');
	ok(parseIPv6('zz::1') === null, 'non-hex rejected');
	ok(parseIPv6('1.2.3.4') === null, 'ipv4 string rejected');
}

console.log('--- constants ---');
eq(MAX_PAYLOAD, 64 * 1024, 'payload cap 64KB');
eq(STATUS_OK, 0, 'status 0x00');
eq(STATUS_BAD, 1, 'status 0x01');
eq(STATUS_FORBIDDEN, 2, 'status 0x02');
eq(STATUS_NOEXIT, 3, 'status 0x03');

console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);
