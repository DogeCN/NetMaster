// Cron 健康检查（PRD §9）。每小时把内置源拉一遍，对每个出口候选做一次真实
// 连通性探测，按 EWMA（α=0.3，延迟:成功率 = 7:3）排序，把前 4 写进 KV。
// 读取端是 race.js 的 KV_RELAY_KEY。
//
// 约束：Workers 免费版 Cron 触发每次运行最多 50 次外部子请求。候选一多就得分
// 批 —— 用 cursor 游标把候选切成每轮 <= budget 条，多轮跑完再排序写 KV。
// last-good-wins：只有本轮至少成功探测到候选才覆盖 KV，否则保留上一轮。
//
// NAT64 已砍（Workers connect() 无 IPv6 出站，见 docs/m0-findings.md），候选
// 只有 ProxyIP 一类。

import { parseRelay, RELAY_TYPE_HTTP_CONNECT } from './proxyip.js';
import { KV_RELAY_KEY, RACE_KV_TOP } from './race.js';


export const CRON_KEY_CURSOR = "cron:cursor";
export const CRON_KEY_PENDING = "cron:pending"; // 本轮累计的探测结果（未排序）
export const CRON_KEY_LAST = "cron:lastRun";

export const EWMA_ALPHA = 0.3;
export const WEIGHT_LATENCY = 7;
export const WEIGHT_SUCCESS = 3;

// 子请求预算：留 2 个余量给 KV 读写。
export const SUBREQUEST_BUDGET = 48;

export const PROBE_TIMEOUT_MS = 3000;

// EWMA 平滑：score = latency(ms) 归一后与成功率加权，越小越好。
export function scoreOf(latencyMs, success) {
	return WEIGHT_LATENCY * latencyMs + WEIGHT_SUCCESS * (1000 / Math.max(success, 0.01));
}

// mergeScore 把新观测并入已有分数（EWMA，α=0.3）。
export function mergeScore(prev, latencyMs, success) {
	const s = scoreOf(latencyMs, success);
	if (prev == null) return s;
	return (1 - EWMA_ALPHA) * prev + EWMA_ALPHA * s;
}

// ---- 候选源 ----

export // 内置源：IPDB 的 bestproxy（纯文本 ip:port，小时级更新）与 CMLiussss 域名型中继。
// 两者互为备份；任一源挂掉都不影响其他源，全挂则用下面的 BUILTIN_RELAYS 兜底。
const RELAY_SOURCES = [
  { url: "https://ipdb.api.030101.xyz/?type=bestproxy", kind: "text" },
];

export const BUILTIN_RELAYS = [
  "ProxyIP.HK.CMLiussss.net:443",
  "ProxyIP.US.CMLiussss.net:443",
  "ProxyIP.JP.CMLiussss.net:443",
  "ProxyIP.SG.CMLiussss.net:443",
  "ProxyIP.TW.CMLiussss.net:443",
  "ProxyIP.DE.CMLiussss.net:443",
];

// parseRelayList 解析纯文本 ip:port / host:port（# 注释、空行跳过）。
export function parseRelayList(text) {
	const out = [];
	const seen = new Set();
	for (const raw of String(text || "").split("\n")) {
		// 行尾注释（"1.2.3.4:1080  # note"）也要剥掉，否则端口解析失败整行被丢
		const line = raw.split("#")[0].trim();
		if (!line) continue;
		const relay = parseRelay(line);
		if (!relay) continue;
		const key = `${relay.host}:${relay.port}`;
		if (seen.has(key)) continue;
		seen.add(key);
		out.push(relay);
	}
	return out;
}

// filterLoopback 剔除回指本 Worker 的条目：回连自身会被平台拒绝（TCP Loop），
// 留着只会白白消耗槽位。
export async function filterLoopback(relays, workerHost) {
	const out = [];
	for (const r of relays) {
		if (workerHost && (r.host === workerHost || r.host.endsWith("." + workerHost))) continue;
		out.push(r);
	}
	return out;
}

// ---- 探测 ----

// probeRelay 完成 CONNECT 握手后写入一个字节，确认隧道真的能载数据。
// 单纯的 connect() 成功不代表中继可用（很多中继 accept 后立刻 RST）。
async function probeRelay(relay) {
	const t0 = Date.now();
	let socket;
	try {
		socket = connect({ hostname: relay.host, port: relay.port });
		await Promise.race([
			socket.opened,
			new Promise((_, rej) => setTimeout(() => rej(new Error("connect timeout")), PROBE_TIMEOUT_MS)),
		]);
		const writer = socket.writable.getWriter();
		await writer.write(
			new TextEncoder().encode(`CONNECT ${relay.probeTarget} HTTP/1.1\r\nHost: ${relay.probeTarget}\r\n\r\n`)
		);
		writer.releaseLock();

		const reader = socket.readable.getReader();
		let buf = "";
		const deadline = Date.now() + PROBE_TIMEOUT_MS;
		for (;;) {
			if (Date.now() > deadline) throw new Error("handshake timeout");
			const { value, done } = await Promise.race([
				reader.read(),
				new Promise((r) => setTimeout(() => r({ value: null, done: false, timedOut: true }), 500)),
			]);
			if (done) throw new Error("eof before handshake");
			if (value) {
				buf += new TextDecoder().decode(value);
				if (buf.includes("\r\n\r\n")) break;
			}
		}
		const statusLine = buf.split("\r\n")[0] || "";
		const ok = /\s2\d\d\s/.test(statusLine);
		return { ok, ms: Date.now() - t0, status: statusLine };
	} catch (e) {
		return { ok: false, ms: Date.now() - t0, status: String(e.message || e) };
	} finally {
		try {
			socket?.close();
		} catch {}
	}
}

// ---- scheduled handler ----

// runCron 每小时一次的健康检查主体。返回本轮统计（写日志用）。
// 分批：cursor 记录进度，pending 累积观测，全部跑完才排序写 KV。
async function runCron(env, ctx, workerHost) {
	const bucketStart = Date.now();

	// 1) 候选：拉源（失败则用内置兜底），剔除回指条目。
	let relays;
	let source = "builtin";
	if (env.KV) {
		const fetched = await fetchRelayPool(env);
		if (fetched.length) {
			relays = fetched;
			source = "fetched";
		}
	}
	if (!relays || !relays.length) relays = BUILTIN_RELAYS.map(parseRelay).filter(Boolean);
	relays = await filterLoopback(relays, workerHost);

	// 2) 分批探测：cursor 断点续跑。
	const cursor = Number((await env.KV?.get(CRON_KEY_CURSOR)) || 0);
	const batch = relays.slice(cursor, cursor + SUBREQUEST_BUDGET);
	const pending = JSON.parse((await env.KV?.get(CRON_KEY_PENDING)) || "[]");

	// 探测目标固定为一个轻量 HTTP 端点；中继按 CONNECT 隧道转发到这里。
	const results = await Promise.all(
		batch.map((r) => probeRelay({ ...r, probeTarget: "cp.cloudflare.com:80" }).then((res) => ({ relay: r, ...res })))
	);
	pending.push(...results.map((r) => ({ host: r.relay.host, port: r.relay.port, ok: r.ok, ms: r.ms })));

	const next = cursor + batch.length;
	if (next < relays.length) {
		await env.KV?.put(CRON_KEY_CURSOR, String(next));
		await env.KV?.put(CRON_KEY_PENDING, JSON.stringify(pending));
		return { batch: batch.length, total: relays.length, done: false, source };
	}

	// 3) 全部跑完：EWMA 排序 → 取前 4 → 写 KV（last-good-wins）
	//
	// last-good-wins 的判定是"本轮至少有一条探测成功"，不是"排序结果非空"：
	// 候选全灭时 rankRelays 照样返回列表（死候选排在后面留着下轮翻身），
	// 若拿列表长度当成功标志，全灭的一轮会用死列表覆盖好数据。
	const scored = rankRelays(pending);
	const okCount = scored.filter((s) => s.success > 0).length;
	if (okCount === 0) {
		await env.KV?.delete(CRON_KEY_CURSOR);
		await env.KV?.delete(CRON_KEY_PENDING);
		return { batch: batch.length, total: relays.length, done: true, source, written: 0, okCount, reason: "all probes failed" };
	}

	const top = scored.filter((s) => s.success > 0).slice(0, RACE_KV_TOP).map((s) => ({ host: s.host, port: s.port, type: RELAY_TYPE_HTTP_CONNECT, ms: Math.round(s.ms), score: Number(s.score.toFixed(1)) }));
	await env.KV?.put(KV_RELAY_KEY, JSON.stringify(top));
	const failSamples = pending.filter((r) => !r.ok).slice(0, 3).map((r) => `${r.host}:${r.port} ${r.ms}ms`);
	await env.KV?.put(CRON_KEY_LAST, JSON.stringify({ at: bucketStart, n: top.length, of: relays.length, ok: okCount, fails: failSamples }));
	await env.KV?.delete(CRON_KEY_CURSOR);
	await env.KV?.delete(CRON_KEY_PENDING);
	return { batch: batch.length, total: relays.length, done: true, source, written: top.length };
}

// rankRelays 把观测并入历史 EWMA 并排序（越小越靠前）。
export function rankRelays(observations) {
	const acc = new Map();
	for (const o of observations) {
		const key = `${o.host}:${o.port}`;
		const prev = acc.get(key) || { host: o.host, port: o.port, ms: 0, n: 0, okN: 0 };
		prev.ms += o.ms;
		prev.n++;
		if (o.ok) prev.okN++;
		acc.set(key, prev);
	}
	const out = [];
	for (const v of acc.values()) {
		const success = v.okN / Math.max(v.n, 1);
		const latency = v.ms / Math.max(v.n, 1);
		const score = mergeScore(null, latency, success);
		out.push({ host: v.host, port: v.port, ms: latency, success, score });
	}
	// 全败的候选排在后面而不是被丢弃：它们可能在下一轮恢复。
	out.sort((a, b) => {
		if ((a.success > 0) !== (b.success > 0)) return a.success > 0 ? -1 : 1;
		return a.score - b.score;
	});
	return out;
}

// fetchRelayPool 拉内置源里的中继池；任一源失败不影响其他源。
async function fetchRelayPool(env) {
	const out = [];
	for (const src of RELAY_SOURCES) {
		try {
			const r = await fetch(src.url, { cf: { cacheTtl: 3600 } });
			if (!r.ok) continue;
			out.push(...parseRelayList(await r.text()));
		} catch {
			// 单源失败静默：还有内置兜底
		}
	}
	return out;
}
