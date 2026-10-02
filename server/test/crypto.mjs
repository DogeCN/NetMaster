// crypto.mjs — v2 鉴权原语单测：HMAC 派生、TS 窗口、常量时间比较。
import { createHmac } from 'node:crypto';
import { authCode, tsFromBytes, tsWithinWindow, safeEqualBytes, utf8, AUTH_LEN, TS_WINDOW_SEC } from '../src/crypto.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

console.log('--- authCode ---');
{
	// 与 node:crypto 的标准 HMAC-SHA256 交叉验证
	const signed = Uint8Array.from([1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 1, 2, 104, 101, 108, 108, 111, 0, 68]);
	const got = await authCode('hunter2', signed);
	const ref = createHmac('sha256', 'hunter2').update(signed).digest().slice(0, AUTH_LEN);
	ok(got.length === AUTH_LEN, 'auth is 16 bytes');
	ok(Buffer.from(got).equals(ref), 'matches node:crypto HMAC-SHA256[:16]');
	const other = await authCode('hunter3', signed);
	ok(!Buffer.from(got).equals(other), 'different password -> different auth');
	const otherMsg = await authCode('hunter2', signed.slice(0, 12));
	ok(!Buffer.from(got).equals(otherMsg), 'different message -> different auth');
}

console.log('--- ts window ---');
{
	const now = Math.floor(Date.now() / 1000);
	ok(tsFromBytes(Uint8Array.from([0, 0, 0, 0, 0, 0, 0, 0])) === 0n, 'tsFromBytes zero');
	const nowBytes = new Uint8Array(8);
	new DataView(nowBytes.buffer).setBigUint64(0, BigInt(now), false);
	ok(tsFromBytes(nowBytes) === BigInt(now), 'tsFromBytes roundtrip');
	ok(tsWithinWindow(BigInt(now), now), 'now within window');
	ok(tsWithinWindow(BigInt(now - TS_WINDOW_SEC), now), 'window edge -300 ok');
	ok(tsWithinWindow(BigInt(now + TS_WINDOW_SEC), now), 'window edge +300 ok');
	ok(!tsWithinWindow(BigInt(now - TS_WINDOW_SEC - 1), now), 'beyond -300 rejected');
	ok(!tsWithinWindow(BigInt(now + TS_WINDOW_SEC + 1), now), 'beyond +300 rejected');
}

console.log('--- safeEqualBytes ---');
{
	ok(safeEqualBytes(Uint8Array.from([1, 2, 3]), Uint8Array.from([1, 2, 3])), 'equal');
	ok(!safeEqualBytes(Uint8Array.from([1, 2, 3]), Uint8Array.from([1, 2, 4])), 'differ');
	ok(!safeEqualBytes(Uint8Array.from([1, 2]), Uint8Array.from([1, 2, 3])), 'length differ');
}

console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);
