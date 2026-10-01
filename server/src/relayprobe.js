/**
 * Ask a target, through a relay, what it would answer.
 *
 * Why this has to exist: a ProxyIP relay is a plain SNI-based reverse proxy for
 * Cloudflare's HTTPS, so reaching the target *from the relay's exit IP* is what
 * decides whether a Cloudflare-fronted site serves us or hands back a 403 — the
 * site sees the relay, not the client. Without a measurement, relay selection is
 * a lottery: one gets bound to the host for the isolate's lifetime, and if it
 * was blocked every request for that host fails until the isolate is recycled.
 *
 * The measurement has to happen inside the Worker: relays are unreachable from
 * most of the world (from a residential connection inside China every foreign
 * relay times out), so probing them from outside proves nothing.
 *
 * How: `fetch` with `cf.resolveOverride`. The URL supplies the target, so SNI and
 * Host are the target's, while the TCP connection goes to the relay — exactly the
 * path a real proxied request takes.
 *
 * The obvious alternative does not work here: `connect(..., {secureTransport:
 * 'on'})` would use whatever hostname we passed to connect(), and the Pages
 * runtime's `cloudflare:sockets` build has no `startTls` export (verified on a
 * deployed instance), so there is no way to open a raw socket to the relay and
 * then handshake with the target's SNI.
 */
import { withTimeout } from './util.js';

/**
 * Bound on the whole probe. Kept short on purpose: this runs inline on a user's
 * first request for a host, so a probe that cannot work on this platform must
 * not cost more than a moment before falling back.
 */
const PROBE_TIMEOUT_MS = 2500;

/**
 * @returns {Promise<number|{status:number, err:string}>} the HTTP status the
 *   target returns through this relay, or 0 when the relay could not deliver the
 *   request at all. With `withReason`, failures also carry a message.
 */
export async function probeTargetViaRelay(fetcher, relay, host, opts = {}) {
	const timeoutMs = opts.timeoutMs || PROBE_TIMEOUT_MS;
	const fail = (err) => (opts.withReason ? { status: 0, err: String(err) } : 0);
	const ok = (status) => (opts.withReason ? { status } : status);

	try {
		const ctl = new AbortController();
		const timer = setTimeout(() => ctl.abort(), timeoutMs);
		// Use the request's own fetcher, never the global: it is what the runtime
		// scopes this request to, and it keeps tests offline. If it has no fetch,
		// report "cannot probe" rather than silently reaching through a different
		// scope (which would also escape this request's egress identity).
		const doFetch = typeof fetcher?.fetch === 'function' ? fetcher.fetch.bind(fetcher) : null;
		if (!doFetch) return fail('fetcher.fetch unavailable');
		let resp;
		try {
			resp = await doFetch(`https://${host}/`, {
				method: 'HEAD',
				redirect: 'manual',
				cf: { resolveOverride: relay },
				signal: ctl.signal,
			});
		} finally {
			clearTimeout(timer);
		}
		return ok(resp.status);
	} catch (err) {
		return fail(err?.message || err);
	}
}

/**
 * Is a probed status good enough to bind a relay to this host?
 *
 * 403 is the interesting one: the relay did its job and reached the target, but
 * the target refused that exit IP. That is precisely the case worth routing
 * around, so it disqualifies the relay. Everything else — including 4xx and 5xx
 * from the site itself — means the path works.
 */
export function relayAnswersCleanly(status) {
	return typeof status === 'number' && status !== 0 && status !== 403;
}
