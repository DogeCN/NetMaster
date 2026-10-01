/**
 * forward.js — WS 之上的多路复用转发器。
 *
 * 协议见 protocol.js。出站 TCP 用 `request.fetcher.connect()`（Workers 的
 * TCP socket API），出口 IP 由 Cloudflare 分配，不可控。
 */

import { parseSessionOpen, buildMuxFrame, parseMuxFrame } from './protocol.js';
import { safeEqualBytes } from './crypto.js';
import { withTimeout } from './util.js';
import { probeTargetViaRelay, relayAnswersCleanly } from './relayprobe.js';

/** Max concurrent sessions per WebSocket. */
const MAX_SESSIONS = 64;

/** Max buffered upstream bytes per session. */
const MAX_QUEUE_BYTES = 8 * 1024 * 1024;

/** Keep an idle WS alive this long so clients can reuse it. */
const IDLE_TIMEOUT_MS = 120000;

/** Bound on waiting for an outbound TCP connect. */
const CONNECT_TIMEOUT_MS = 15000;

/** WS close code used when the auth frame does not match. */
const CLOSE_UNAUTHORIZED = 1008;

/**
 * 中继（ProxyIP）兜底列表。
 *
 * Cloudflare 禁止 Worker 出站连 Cloudflare 自己的 IP 段，所以任何 Cloudflare
 * 承载的站点直连不可达（实测 ~10ms 内被拒）。中继是第三方主机，把 Cloudflare
 * 的 HTTPS 反代出去：我们拨它，客户端字节原样通过（TLS ClientHello 仍带真实
 * SNI），由它转发。
 *
 * 正式列表从社区源动态获取（见 RELAY_SOURCE），这里只做源不可达时的兜底。
 * 直连永远先试：被拒的目标 ~10ms 就返回，不需要中继的目标永远不多付一跳。
 */
const FALLBACK_RELAYS = [
	'ProxyIP.HK.CMLiussss.net',
	'ProxyIP.JP.CMLiussss.net',
	'ProxyIP.KR.CMLiussss.net',
	'ProxyIP.DE.tp2024.CMLiussss.net',
	'ProxyIP.Aliyun.CMLiussss.net',
	'ProxyIP.Oracle.CMLiussss.net',
	'ProxyIP.DigitalOcean.CMLiussss.net',
	'ProxyIP.Vultr.CMLiussss.net',
	'ProxyIP.Multacom.CMLiussss.net',
];

/**
 * 中继列表的社区源：反代优选 IP，随上游更新而更新。硬编列表会过期，
 * 硬编"从哪拿列表"不会。探测与亲和机制保证拿到手的每一项都真可用。
 */
const RELAY_SOURCE = 'https://ipdb.api.030101.xyz/?type=bestproxy';

/** 动态列表在本 isolate 内的缓存时长。 */
const RELAY_SOURCE_TTL_MS = 60 * 60 * 1000;

let relayListCache = { at: 0, list: null };

/**
 * 当前生效的中继列表。override 为数组时直接采用（测试注入），否则取
 * 动态列表（1 小时缓存），失败并上兜底。
 * @param {string[]|undefined} override
 * @returns {Promise<string[]>}
 */
export async function currentRelays(override) {
	if (Array.isArray(override)) return override;
	if (relayListCache.list && Date.now() - relayListCache.at < RELAY_SOURCE_TTL_MS) {
		return relayListCache.list;
	}
	let fresh = [];
	try {
		const res = await fetch(RELAY_SOURCE, { signal: AbortSignal.timeout(5000) });
		fresh = res.ok ? parseRelayList(await res.text()) : [];
	} catch (_) {
		// 源不可达：下面用兜底列表，不影响可用性。
	}
	const seen = new Set();
	relayListCache = {
		at: Date.now(),
		list: [...fresh, ...FALLBACK_RELAYS].filter((r) => !seen.has(r) && seen.add(r)),
	};
	return relayListCache.list;
}

/**
 * 宽容解析中继源返回的文本：每行一个 `host[:port]`，可带 `#名字` 后缀，
 * 也可能混着 HTML。只收 443（中继语义就是反代 Cloudflare 的 443），
 * 裸 IPv4 包成 sslip.io 主机名 —— 探测走 resolveOverride，要求主机名。
 */
export function parseRelayList(text) {
	const out = [];
	const seen = new Set();
	for (const rawLine of String(text || '').split(/\r?\n/)) {
		const line = rawLine.split('#')[0].trim();
		if (!line) continue;
		let host = line;
		const i = line.lastIndexOf(':');
		if (i > 0 && /^\d{1,5}$/.test(line.slice(i + 1))) {
			if (Number(line.slice(i + 1)) !== 443) continue;
			host = line.slice(0, i);
		}
		if (!/^[a-zA-Z0-9.-]+$/.test(host)) continue;
		if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host)) {
			host = `${host}.sslip.io`;
		} else if (!host.includes('.')) {
			continue;
		}
		if (seen.has(host)) continue;
		seen.add(host);
		out.push(host);
	}
	return out;
}

/**
 * host -> relay hostname that last worked for it.
 *
 * 与客户端的域名亲和（"同站同出口"）同一思想的下一层：选中一次就记住。
 * 中继的差异在于哪些站点接受它的出口 IP —— 一个中继被某站拉黑，另一个未必，
 * 所以按域名而不是全局记。模块级持有，isolate 存活期内有效；isolate 重启后
 * 由 D1 持久层接住（recallRelay），再不行就重新探测。
 */
const relayAffinity = new Map();
const RELAY_AFFINITY_MAX = 512;

function rememberRelay(host, relay) {
	if (relayAffinity.has(host)) relayAffinity.delete(host);
	relayAffinity.set(host, relay);
	if (relayAffinity.size > RELAY_AFFINITY_MAX) {
		// Drop the oldest entry (Map preserves insertion order).
		relayAffinity.delete(relayAffinity.keys().next().value);
	}
}

/** Build the ordered list of endpoints to try for a session's target. */
function buildTargets(r, relays) {
	const out = [{ hostname: r.host, port: r.port }];
	// ProxyIP relays Cloudflare's HTTPS service, so only port 443 is meaningful.
	if (r.port === 443 && relays.length) {
		// Try the relay this host worked with before, then the rest in random
		// order so a dead relay cannot pin a host forever.
		const bound = relayAffinity.get(r.host);
		const rest = shuffled(relays.filter((ip) => ip !== bound));
		const ordered = bound ? [bound, ...rest] : rest;
		for (const ip of ordered) out.push({ hostname: ip, port: 443, relay: true });
	}
	return out;
}

/** Fisher-Yates on a copy: random order so a slow/dead relay is not always first. */
function shuffled(list) {
	const a = list.slice();
	for (let i = a.length - 1; i > 0; i--) {
		const j = Math.floor(Math.random() * (i + 1));
		[a[i], a[j]] = [a[j], a[i]];
	}
	return a;
}

/** How many relays to ask when picking one for a host we have no binding for. */
const RELAY_PROBE_CANDIDATES = 4;

/** Overall ceiling on relay selection, so one request cannot spend minutes probing. */
const RELAY_PICK_BUDGET_MS = 3000;

export class Forwarder {
	/**
	 * @param {Request} request
	 * @param {WebSocket} ws server-side socket (we send on this, client receives)
	 * @param {Uint8Array} authBytes expected auth frame (md5 of PASSWORD)
	 * @param {{log?:Function, db?:object, ctx?:object, relays?:string[]}} [opts]
	 */
	constructor(request, ws, authBytes, opts = {}) {
		this.request = request;
		this.ws = ws;
		this.authBytes = authBytes;
		this.authed = false;
		// request.fetcher.connect is the Workers outbound-TCP API; without it
		// nothing can be forwarded, so fail loudly rather than at first use.
		const fetcher = request?.fetcher;
		if (!fetcher || typeof fetcher.connect !== 'function') {
			throw new Error('request.fetcher.connect unavailable in this runtime');
		}
		// Keep the receiver at fetch time: some runtimes require fetcher.connect
		// to be called with fetcher as `this`.
		this.connect = (options, init) => (init === undefined
			? fetcher.connect(options)
			: fetcher.connect(options, init));
		this.fetcher = fetcher;
		/** D1 database + execution context, used to persist relay affinity so a
		 *  fresh isolate does not have to re-learn it (and re-roll the dice). */
		this.db = opts.db && typeof opts.db.prepare === 'function' ? opts.db : null;
		this.ctx = opts.ctx || null;
		/** 测试注入的中继列表；生产为 undefined，走动态获取。 */
		this.relaysOverride = opts.relays;
		/** @type {Map<string, object>} sessionKey -> session */
		this.sessions = new Map();
		/** Hosts currently being probed for a relay, so concurrent sessions for
		 *  the same host do not each run their own probe round. */
		this.probing = new Map();
		this.buf = null;      // 跨消息累积的未解析字节
		this.closed = false;
		this.idleTimer = null;
		this.log = opts.log || (() => {});
	}

	start() {
		this.ws.binaryType = 'arraybuffer';
		this.ws.addEventListener('message', (ev) => {
			this.onMessage(ev.data).catch((e) => this.log(`[fwd] ${e?.message || e}`));
		});
		this.ws.addEventListener('close', () => { this.log('[fwd] ws closed by peer'); this.shutdown(); });
		this.ws.addEventListener('error', () => { this.log('[fwd] ws error'); this.shutdown(); });
	}

	shutdown() {
		if (this.closed) return;
		this.closed = true;
		if (this.idleTimer) { clearTimeout(this.idleTimer); this.idleTimer = null; }
		for (const s of this.sessions.values()) this.kill(s);
		this.sessions.clear();
		try { this.ws.close(); } catch (_) {}
	}

	kill(s) {
		if (!s) return;
		s.dead = true;
		try { s.socket?.close?.(); } catch (_) {}
		s.socket = null;
	}

	async send(bytes) {
		if (this.closed) return;
		try {
			await this.ws.send(bytes);
		} catch (_) { /* client gone */ }
	}

	async onMessage(data) {
		if (this.closed) return;
		const bytes = toBytes(data);
		if (!bytes || !bytes.byteLength) return;

		// 握手：第一条消息必须恰为 16 字节 auth。措辞从严（不尝试从更长的
		// 消息里剥前 16 字节），因为宽松只会制造一种客户端 bug：auth 和首帧
		// 被意外合并发送时静默通过，而不是被立刻发现。
		if (!this.authed) {
			if (safeEqualBytes(bytes, this.authBytes)) {
				this.authed = true;
				return;
			}
			this.log('[fwd] auth mismatch, closing');
			this.closed = true;
			for (const s of this.sessions.values()) this.kill(s);
			this.sessions.clear();
			try { this.ws.close(CLOSE_UNAUTHORIZED, 'unauthorized'); } catch (_) {}
			return;
		}

		// Accumulate until we have a complete mux frame (first frame may be split).
		this.buf = this.buf ? concat(this.buf, bytes) : bytes;
		const frame = parseMuxFrame(this.buf);
		if (!frame) return;
		// Consume the frame; any trailing bytes stay for the next message.
		this.buf = frame.consumed >= this.buf.byteLength
			? null
			: this.buf.subarray(frame.consumed);

		const key = keyOf(frame.sessionId);
		let s = this.sessions.get(key);

		if (!s) {
			// Opening frame for a new session: payload is a session-open header.
			const parsed = parseSessionOpen(frame.payload);
			if (parsed.status === 'need_more') {
				await this.sendControl(frame.sessionId, 'incomplete session-open header');
				return;
			}
			if (parsed.status !== 'ok') {
				await this.sendControl(frame.sessionId, `bad session-open header: ${parsed.reason || 'unknown'}`);
				return;
			}
			s = this.newSession(frame.sessionId);
			if (!s) {
				await this.sendControl(frame.sessionId, 'session rejected (duplicate id or session cap)');
				return;
			}
			await this.openUpstream(s, parsed.result);
			if (parsed.result.payload?.byteLength) this.write(s, parsed.result.payload);
			return;
		}

		if (frame.payload?.byteLength) this.write(s, frame.payload);
	}

	newSession(id) {
		if (!id || !id.length) return null;
		this.cancelIdleClose();
		const key = keyOf(id);
		if (this.sessions.has(key)) return null; // duplicate id
		if (this.sessions.size >= MAX_SESSIONS) {
			this.log(`[fwd] session cap ${MAX_SESSIONS} reached, dropping ${key}`);
			return null;
		}
		const s = { id, key, socket: null, opening: null, queue: [], queued: 0, dead: false };
		this.sessions.set(key, s);
		return s;
	}

	/**
	 * Choose a relay for a host we have no binding for yet.
	 *
	 * Relay choice decides the *exit IP* the target sees, and therefore whether a
	 * Cloudflare-fronted site serves us or answers 403. Picking one at random is
	 * a lottery: a blocked relay gets bound to the host for this isolate's whole
	 * lifetime. So ask a few candidates directly and prefer one that answers.
	 */
	async pickRelayFor(host, relays) {
		const bound = relayAffinity.get(host);
		if (bound) return bound;

		let inflight = this.probing.get(host);
		if (!inflight) {
			inflight = (async () => {
				// A previous isolate may already have learned this; D1 is what stops
				// every fresh isolate from rolling the dice again (and getting it
				// wrong often enough to look like flakiness).
				const remembered = await this.recallRelay(host);
				if (remembered && relays.includes(remembered)) {
					relayAffinity.set(host, remembered);
					this.log(`[fwd] ${host}: relay ${remembered} (from D1)`);
					return remembered;
				}

				const deadline = Date.now() + RELAY_PICK_BUDGET_MS;
				const tried = [];
				for (const relay of shuffled(relays).slice(0, RELAY_PROBE_CANDIDATES)) {
					if (Date.now() > deadline) {
						this.log(`[fwd] ${host}: relay probe budget spent ${JSON.stringify(tried)}`);
						break;
					}
					const status = await probeTargetViaRelay(this.fetcher, relay, host);
					tried.push({ relay, status });
					if (relayAnswersCleanly(status)) {
						rememberRelay(host, relay);
						this.rememberRelayDurably(host, relay);
						this.log(`[fwd] ${host}: relay ${relay} answers ${status}`);
						return relay;
					}
				}
				this.log(`[fwd] ${host}: no relay answered cleanly ${JSON.stringify(tried)}`);
				return null;
			})().finally(() => this.probing.delete(host));
			this.probing.set(host, inflight);
		}
		return inflight;
	}

	/** Read a previously learned binding from D1. */
	async recallRelay(host) {
		if (!this.db) return null;
		try {
			const row = await this.db
				.prepare('SELECT relay FROM relay_binding WHERE host = ?')
				.bind(host)
				.first();
			return row?.relay || null;
		} catch (_) {
			// D1 missing or failing: fall back to probing, which still works.
			return null;
		}
	}

	/**
	 * Persist a learned binding so other isolates start from it.
	 *
	 * Best-effort and off the response path: a fresh isolate rolls the dice again
	 * if this write is lost, which costs one bad session, so it must never delay or
	 * fail the request that triggered it.
	 */
	rememberRelayDurably(host, relay) {
		if (!this.db) return;
		const run = this.db
			.prepare(
				'INSERT INTO relay_binding (host, relay, updated_at) VALUES (?1, ?2, ?3) ' +
				'ON CONFLICT(host) DO UPDATE SET relay = excluded.relay, updated_at = excluded.updated_at')
			.bind(host, relay, Date.now())
			.run()
			.catch((err) => this.log(`[fwd] D1 write ${host} failed: ${err?.message || err}`));
		if (this.ctx && typeof this.ctx.waitUntil === 'function') this.ctx.waitUntil(run);
	}

	/**
	 * Forget a binding whose relay turned out dead (zero-byte stream). Same
	 * best-effort contract as rememberRelayDurably: losing the delete only costs
	 * one extra bad session before the next forget.
	 */
	forgetRelayDurably(host, relay) {
		if (!this.db) return;
		const run = this.db
			.prepare('DELETE FROM relay_binding WHERE host = ? AND relay = ?')
			.bind(host, relay)
			.run()
			.catch((err) => this.log(`[fwd] D1 delete ${host} failed: ${err?.message || err}`));
		if (this.ctx && typeof this.ctx.waitUntil === 'function') this.ctx.waitUntil(run);
	}

	async openUpstream(s, r) {
		if (s.opening) return s.opening;
		s.opening = (async () => {
			// 中继列表只在需要时取（443 才有中继语义），非 443 流量不多付这一次。
			const relays = r.port === 443 ? await currentRelays(this.relaysOverride) : [];
			// No relay bound for this host yet: pick one deliberately rather than at
			// random, because the relay's exit IP decides whether a
			// Cloudflare-fronted site serves us or hands back a 403.
			if (r.port === 443 && relays.length && !relayAffinity.get(r.host)) {
				await this.pickRelayFor(r.host, relays);
			}
			const targets = buildTargets(r, relays);
			let lastErr = null;
			for (const t of targets) {
				let sock = null;
				try {
					sock = this.connect({ hostname: t.hostname, port: t.port });
					// A socket whose opened promise neither resolves nor rejects
					// would otherwise hang this session forever; bound the wait.
					await withTimeout(sock.opened, CONNECT_TIMEOUT_MS,
						`connect ${t.hostname}:${t.port} timed out`);
					s.socket = sock;
					// Remember which relay worked for this host so later requests
					// for the same domain skip the ones that fail for it.
					if (t.relay) {
						s.host = r.host;
						s.relay = t.hostname;
						rememberRelay(r.host, t.hostname);
						this.rememberRelayDurably(r.host, t.hostname);
					}
					// Tell the client the session is actually up before any data.
					await this.sendControl(s.id, '');
					this.pumpDown(s);
					this.log(`[fwd] ${r.host}:${r.port} via ${t.hostname}:${t.port}${t.relay ? ' (proxyip)' : ''} sid=${s.key}`);
					this.flush(s);
					return true;
				} catch (err) {
					lastErr = err;
					try { sock?.close?.(); } catch (_) {}
				}
			}
			const msg = lastErr?.message || String(lastErr);
			const tried = targets.map((t) => `${t.hostname}:${t.port}`).join(', ');
			this.log(`[fwd] connect ${r.host}:${r.port} failed: ${msg}`);
			// Report every attempted endpoint: without it a rejected port is
			// indistinguishable from an unreachable one.
			await this.sendControl(s.id, `connect ${r.host}:${r.port} failed (tried ${tried}): ${msg}`);
			this.drop(s);
			return false;
		})();
		return s.opening;
	}

	/**
	 * 控制帧：[0x00][idLen][sessionId][utf-8 文本]。
	 * 会话帧的 id 恒为 ≥1 字节且首字节非 0，故 0x00 可作为控制帧标记。
	 */
	async sendControl(sessionId, text) {
		const body = new TextEncoder().encode(text);
		const sid = sessionId || new Uint8Array(0);
		const out = new Uint8Array(2 + sid.length + body.length);
		let i = 0;
		out[i++] = 0;
		out[i++] = sid.length;
		out.set(sid, i); i += sid.length;
		out.set(body, i);
		await this.send(out);
	}

	/** upstream -> client, wrapped in this session's mux frame. */
	async pumpDown(s) {
		const sock = s.socket;
		if (!sock?.readable) {
			await this.sendControl(s.id, 'upstream socket has no readable stream');
			this.drop(s);
			return;
		}
		let reader;
		let bytesOut = 0;
		try {
			reader = sock.readable.getReader();
			for (;;) {
				const { done, value } = await reader.read();
				if (done || s.dead) break;
				if (value?.byteLength) {
					bytesOut += value.byteLength;
					await this.send(buildMuxFrame(s.id, value));
				}
			}
		} catch (err) {
			await this.sendControl(s.id, `downlink error after ${bytesOut}B: ${err?.message || err}`);
		} finally {
			try { reader?.releaseLock(); } catch (_) {}
			// If the upstream closed without ever sending anything, tell the
			// client — an empty stream is otherwise indistinguishable from a
			// dead session.
			if (bytesOut === 0 && !s.dead) {
				await this.sendControl(s.id, 'upstream closed without sending data');
				// 这条隧道经中继建立却一个字节都没回来：多半是中继半死
				// （TCP 能通、转发不工作）。探测和 D1 记的都是它 —— 忘掉，
				// 下一次（可能就是客户端的透明重试）重新探测别的中继。
				if (s.relay) {
					relayAffinity.delete(s.host);
					this.forgetRelayDurably(s.host, s.relay);
					this.log(`[fwd] ${s.host}: relay ${s.relay} delivered nothing — forgotten`);
				}
			}
			this.drop(s);
		}
	}

	/** Queue or send payload upstream. */
	write(s, payload) {
		if (s.dead) return;
		if (!s.socket) {
			s.queue.push(payload);
			s.queued += payload.byteLength;
			if (s.queued > MAX_QUEUE_BYTES) { this.log(`[fwd] queue overflow sid=${s.key}`); this.drop(s); }
			return;
		}
		try {
			const w = s.socket.writable.getWriter();
			w.write(payload).catch(() => {}).finally(() => {
				try { w.releaseLock(); } catch (_) {}
			});
		} catch (_) {
			this.drop(s);
		}
	}

	flush(s) {
		if (!s.queue.length) return;
		const q = s.queue;
		s.queue = [];
		s.queued = 0;
		for (const p of q) this.write(s, p);
	}

	drop(s) {
		if (!s || s.dead) return;
		this.kill(s);
		this.sessions.delete(s.key);
		// Keep the WebSocket alive briefly after the last session ends: clients
		// multiplex many short-lived connections over one WS, so tearing it down
		// eagerly would force a fresh handshake for the next request. Close only
		// after an idle timeout with no sessions.
		if (this.sessions.size === 0) this.scheduleIdleClose();
	}

	scheduleIdleClose() {
		if (this.idleTimer) return;
		this.idleTimer = setTimeout(() => {
			this.idleTimer = null;
			if (this.sessions.size === 0) this.shutdown();
		}, IDLE_TIMEOUT_MS);
	}

	cancelIdleClose() {
		if (this.idleTimer) {
			clearTimeout(this.idleTimer);
			this.idleTimer = null;
		}
	}
}

function toBytes(data) {
	if (data instanceof ArrayBuffer) return new Uint8Array(data);
	if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
	return null;
}

function concat(a, b) {
	const out = new Uint8Array(a.byteLength + b.byteLength);
	out.set(a, 0);
	out.set(b, a.byteLength);
	return out;
}

function keyOf(id) {
	let s = '';
	for (let i = 0; i < id.length; i++) s += id[i].toString(16).padStart(2, '0');
	return s;
}
