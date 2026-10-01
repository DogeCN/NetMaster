/**
 * MD5 correctness test against known RFC 1321 vectors, plus the auth
 * derivation. If MD5 is wrong, client and worker derive different auth
 * bytes and every connection is closed as unauthorized.
 */
import { md5Hex, authBytes, safeEqualBytes, bytesToHex } from '../src/crypto.js';

let pass = 0, fail = 0;
const eq = (a, b, n) => { if (a === b) { pass++; console.log('  PASS ' + n); } else { fail++; console.log(`  FAIL ${n} got=${a} want=${b}`); } };

console.log('--- MD5 known vectors ---');
eq(await md5Hex(''), 'd41d8cd98f00b204e9800998ecf8427e', 'empty string');
eq(await md5Hex('a'), '0cc175b9c0f1b6a831c399e269772661', '"a"');
eq(await md5Hex('abc'), '900150983cd24fb0d6963f7d28e17f72', '"abc"');
eq(await md5Hex('message digest'), 'f96b697d7cb7938d525a2f31aaf161d0', '"message digest"');
eq(await md5Hex('abcdefghijklmnopqrstuvwxyz'), 'c3fcd3d76192e4007dfb496cca67e13b', 'alphabet');
eq(await md5Hex('ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'), 'd174ab98d277d9f5a5611c2c9f419d9f', 'alnum');
eq(await md5Hex('12345678901234567890123456789012345678901234567890123456789012345678901234567890'), '57edf4a22be3c955ac49da2e2107b67a', '80-digit string');

console.log('--- auth derivation ---');
eq(bytesToHex(authBytes('abc')), '900150983cd24fb0d6963f7d28e17f72', 'auth = md5(utf8(password))');
eq(bytesToHex(authBytes('abc')), bytesToHex(authBytes('abc')), 'deterministic');
eq(authBytes('abc').length, 16, '16 bytes');
ok(safeEqualBytes(authBytes('abc'), authBytes('abc')) === true, 'safeEqualBytes true');
ok(safeEqualBytes(authBytes('abc'), authBytes('abd')) === false, 'safeEqualBytes false');
ok(safeEqualBytes(authBytes('abc'), new Uint8Array(16)) === false, 'safeEqualBytes length mismatch');

function ok(c, n) { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n); } }

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
