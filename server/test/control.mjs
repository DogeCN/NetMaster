/**
 * Verifies the control-frame path: when the upstream TCP connect fails, the
 * worker must tell the client which session failed and why. Without this the
 * client only sees "no data" and cannot distinguish a bad target from a bad
 * handshake.
 */
import { WebSocketServer, WebSocket } from 'ws';
import { Forwarder } from '../src/forward.js';
import { authBytes } from '../src/crypto.js';
import { buildSessionOpen, buildMuxFrame } from '../src/protocol.js';

const PASSWORD = 'control-password';
const AUTH = authBytes(PASSWORD);
const UB = AUTH;

// A fetcher whose connect always fails, to force the error path.
const failingRequest = {
	fetcher: {
		connect() { throw new Error('TEST-CONNECT-FAIL'); },
	},
};

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

const wss = new WebSocketServer({ port: 0 });
await new Promise((r) => wss.once('listening', r));
const port = wss.address().port;

wss.on('connection', (ws) => {
	new Forwarder(failingRequest, ws, UB, { log: () => {}, relays: [] }).start();
});

const client = new WebSocket('ws://127.0.0.1:' + port);
await new Promise((r, j) => { client.once('open', r); client.once('error', j); });
const frames = [];
client.on('message', (d) => frames.push(Buffer.from(d)));

client.send(Buffer.from(AUTH));
await new Promise((r) => setTimeout(r, 100));

const sid = Uint8Array.from([0xde, 0xad, 0xbe, 0xef]);
client.send(Buffer.from(buildMuxFrame(sid, buildSessionOpen({
	host: 'example.com', port: 443,
}))));
await new Promise((r) => setTimeout(r, 600));

ok(frames.length > 0, 'client received a control frame', `frames=${frames.length}`);
if (frames.length) {
	const f = frames[0];
	ok(f[0] === 0, 'tagged as control (0x00)', f[0].toString(16));
	const sidLen = f[1];
	ok(sidLen === 4, 'carries session id length', String(sidLen));
	ok(f.slice(2, 2 + sidLen).toString('hex') === 'deadbeef', 'carries the right session id', f.slice(2, 6).toString('hex'));
	const msg = f.slice(2 + sidLen).toString();
	ok(msg.includes('TEST-CONNECT-FAIL'), 'carries the failure reason', msg);
	ok(msg.includes('example.com:443'), 'names the target that failed', msg);
}

console.log(`\n${pass} passed, ${fail} failed`);
client.close();
wss.close();
process.exit(fail ? 1 : 0);
