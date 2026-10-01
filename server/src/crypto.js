/**
 * crypto.js — MD5 与常量时间比较。
 *
 * Cloudflare Workers 的 SubtleCrypto 不支持 MD5（只有 SHA 系），所以 MD5 在这里
 * 用纯 JS 实现。这也让本模块可以在 Node 测试里直接用（WebCrypto 同样拒绝 MD5）。
 *
 * 鉴权方案：PASSWORD（部署者经 GitHub Secrets 提供）→ auth = md5(utf8(PASSWORD))，
 * 客户端与 Worker 各自派生，网络上只出现 16 字节派生值。派生值在 TLS 之内传输，
 * 与旧 UUID 方案是同一安全等级，但不再需要 D1 生成/存储，也没有默认值可以推算。
 */

import { bytesToHex } from './util.js';

// ---- pure JS MD5 (RFC 1321) ----

const S = [
	7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
	5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
	4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
	6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
];

const K = new Uint32Array(64);
for (let i = 0; i < 64; i++) K[i] = Math.floor(Math.abs(Math.sin(i + 1)) * 4294967296) >>> 0;

/**
 * MD5 of a byte array.
 * @param {Uint8Array} msg
 * @returns {Uint8Array} 16-byte digest
 */
export function md5Bytes(msg) {
	const origLen = msg.length;
	// pad to 56 mod 64
	const withOne = origLen + 1;
	const padded = new Uint8Array(((withOne + 8) >> 6 << 6) + 64);
	padded.set(msg);
	padded[origLen] = 0x80;
	const bitLen = origLen * 8;
	const dv = new DataView(padded.buffer);
	dv.setUint32(padded.length - 8, bitLen >>> 0, true);
	dv.setUint32(padded.length - 4, Math.floor(bitLen / 4294967296), true);

	let a0 = 0x67452301, b0 = 0xefcdab89, c0 = 0x98badcfe, d0 = 0x10325476;
	const M = new Uint32Array(16);

	for (let chunk = 0; chunk < padded.length; chunk += 64) {
		for (let i = 0; i < 16; i++) M[i] = dv.getUint32(chunk + i * 4, true);
		let A = a0, B = b0, C = c0, D = d0;
		for (let i = 0; i < 64; i++) {
			let F, g;
			if (i < 16) { F = (B & C) | (~B & D); g = i; }
			else if (i < 32) { F = (D & B) | (~D & C); g = (5 * i + 1) % 16; }
			else if (i < 48) { F = B ^ C ^ D; g = (3 * i + 5) % 16; }
			else { F = C ^ (B | ~D); g = (7 * i) % 16; }
			F = (F + A + K[i] + M[g]) >>> 0;
			A = D; D = C; C = B;
			B = (B + ((F << S[i]) | (F >>> (32 - S[i])))) >>> 0;
		}
		a0 = (a0 + A) >>> 0; b0 = (b0 + B) >>> 0; c0 = (c0 + C) >>> 0; d0 = (d0 + D) >>> 0;
	}

	const out = new Uint8Array(16);
	const odv = new DataView(out.buffer);
	odv.setUint32(0, a0, true); odv.setUint32(4, b0, true);
	odv.setUint32(8, c0, true); odv.setUint32(12, d0, true);
	return out;
}

/**
 * MD5 hex digest of a UTF-8 string.
 * @param {string} msg
 * @returns {string} lowercase hex
 */
export function md5Hex(msg) {
	return bytesToHex(md5Bytes(new TextEncoder().encode(msg)));
}

/**
 * 鉴权派生：PASSWORD → 16 字节 auth。两端各算一遍，网络只见派生值。
 * @param {string} password
 * @returns {Uint8Array}
 */
export function authBytes(password) {
	return md5Bytes(new TextEncoder().encode(String(password)));
}

export { bytesToHex };

/**
 * Constant-time comparison for secrets.
 */
export function safeEqual(a, b) {
	if (typeof a !== 'string' || typeof b !== 'string') return false;
	if (a.length !== b.length) return false;
	let diff = 0;
	for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
	return diff === 0;
}

/**
 * Constant-time comparison of two equal-length byte arrays.
 * @param {Uint8Array} a
 * @param {Uint8Array} b
 * @returns {boolean}
 */
export function safeEqualBytes(a, b) {
	if (!(a instanceof Uint8Array) || !(b instanceof Uint8Array)) return false;
	if (a.length !== b.length) return false;
	let diff = 0;
	for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
	return diff === 0;
}
