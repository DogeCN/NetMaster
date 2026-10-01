/**
 * protocol.js — NetMaster 内部协议（两端都是我们的，不存在兼容负担）。
 *
 * 传输：一条 WebSocket（路径 /），全部为二进制消息。
 *
 *   消息 1（握手）  16 字节 auth = md5(utf8(PASSWORD))。
 *                   不匹配 → close(1008)。之后不再有鉴权开销。
 *   会话开帧        mux 帧，payload = [port u16 BE][addrType][addr][初始数据]
 *   数据帧          mux 帧，payload = 原始字节
 *   控制帧          [0x00][idLen][sessionId][utf-8 文本]，空文本 = 会话就绪
 *
 * mux 帧是唯一的复用机制：[idLen][sessionId][payload]，一条 WS 上并发跑多个
 * 会话。控制帧借用了 idLen=0 这个会话帧不可能出现的形态（会话 id 恒 ≥1 字节
 * 且首字节非 0）。
 */

export const ADDR_IPV4 = 1;
export const ADDR_DOMAIN = 2;
export const ADDR_IPV6 = 3;

/**
 * Parse a session-open payload.
 * @param {Uint8Array} data
 * @returns {{status:'ok'|'need_more'|'invalid', reason?:string, result?:object}}
 */
export function parseSessionOpen(data) {
	const len = data.byteLength;
	if (len < 3) return { status: 'need_more' };

	const port = (data[0] << 8) | data[1];
	const addrType = data[2];
	const addrIndex = 3;

	let headerLen = -1;
	let host = '';

	if (addrType === ADDR_IPV4) {
		if (len < addrIndex + 4) return { status: 'need_more' };
		host = `${data[addrIndex]}.${data[addrIndex + 1]}.${data[addrIndex + 2]}.${data[addrIndex + 3]}`;
		headerLen = addrIndex + 4;
	} else if (addrType === ADDR_DOMAIN) {
		if (len < addrIndex + 1) return { status: 'need_more' };
		const dLen = data[addrIndex];
		if (len < addrIndex + 1 + dLen) return { status: 'need_more' };
		host = new TextDecoder().decode(data.subarray(addrIndex + 1, addrIndex + 1 + dLen));
		headerLen = addrIndex + 1 + dLen;
	} else if (addrType === ADDR_IPV6) {
		// Go 的 net.SplitHostPort 会剥掉 IPv6 字面量的方括号，所以客户端对
		// [2606:4700::1111]:443 这类目标发的是 atyp 3。
		if (len < addrIndex + 16) return { status: 'need_more' };
		const parts = [];
		for (let i = 0; i < 8; i++) {
			const base = addrIndex + i * 2;
			parts.push(((data[base] << 8) | data[base + 1]).toString(16));
		}
		host = parts.join(':');
		headerLen = addrIndex + 16;
	} else {
		return { status: 'invalid', reason: 'bad address type' };
	}

	if (!host) return { status: 'invalid', reason: 'empty host' };
	if (!port) return { status: 'invalid', reason: 'empty port' };

	return {
		status: 'ok',
		result: {
			host,
			port,
			payload: data.subarray(headerLen),
		},
	};
}

/**
 * Serialize a session-open payload (client side and tests).
 * @param {{host:string, port:number, payload?:Uint8Array}} o
 * @returns {Uint8Array}
 */
export function buildSessionOpen(o) {
	const { host, port, payload = new Uint8Array(0) } = o;
	const nameBytes = new TextEncoder().encode(host);
	let addr;
	const isV4 = /^(\d{1,3}\.){3}\d{1,3}$/.test(host);
	if (isV4) {
		addr = Uint8Array.from(host.split('.').map(Number));
	} else if (host.includes(':')) {
		// IPv6 字面量：与 Go 客户端一致，按 16 字节二进制编码。
		addr = Uint8Array.from(ip6Bytes(host));
	} else {
		addr = new Uint8Array(1 + nameBytes.length);
		addr[0] = nameBytes.length;
		addr.set(nameBytes, 1);
	}
	const addrType = isV4 ? ADDR_IPV4 : host.includes(':') ? ADDR_IPV6 : ADDR_DOMAIN;
	const out = new Uint8Array(3 + addr.length + payload.length);
	let i = 0;
	out[i++] = (port >> 8) & 0xff;
	out[i++] = port & 0xff;
	out[i++] = addrType;
	out.set(addr, i); i += addr.length;
	out.set(payload, i);
	return out;
}

function ip6Bytes(host) {
	// 展开 ::（RFC 4291）：左侧组从头填，右侧组从尾填，中间补零。
	let left = host, right = '';
	if (host.includes('::')) [left, right] = host.split('::');
	const l = left ? left.split(':') : [];
	const r = right ? right.split(':') : [];
	if (l.length + r.length > 8) throw new Error(`bad ipv6 literal: ${host}`);
	const groups = new Array(8).fill(0);
	for (let i = 0; i < l.length; i++) groups[i] = parseInt(l[i], 16) || 0;
	for (let i = 0; i < r.length; i++) groups[8 - r.length + i] = parseInt(r[i], 16) || 0;
	const out = new Uint8Array(16);
	for (let i = 0; i < 8; i++) {
		out[i * 2] = groups[i] >> 8;
		out[i * 2 + 1] = groups[i] & 0xff;
	}
	return out;
}

/**
 * Frame a payload for an established session: [idLen][sessionId][payload].
 */
export function buildMuxFrame(sessionId, payload) {
	const out = new Uint8Array(1 + sessionId.length + payload.length);
	out[0] = sessionId.length;
	out.set(sessionId, 1);
	out.set(payload, 1 + sessionId.length);
	return out;
}

/**
 * Parse a mux frame header from the head of a buffer.
 * The frame is [idLen][sessionId][payload...]; payload runs to the end of the
 * buffer, so `consumed` is reported for callers that must handle trailing bytes
 * (multiple frames arriving coalesced in one WS message).
 * @param {Uint8Array} buf
 * @returns {{sessionId:Uint8Array, payload:Uint8Array, consumed:number}|null}
 */
export function parseMuxFrame(buf) {
	if (buf.byteLength < 2) return null;
	const idLen = buf[0];
	if (idLen < 1) return null;
	const headerLen = 1 + idLen;
	if (buf.byteLength < headerLen) return null;
	return {
		sessionId: buf.slice(1, headerLen),
		payload: buf.subarray(headerLen),
		consumed: buf.byteLength,
	};
}
