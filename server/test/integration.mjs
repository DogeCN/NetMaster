/**
 * integration.mjs — end-to-end test of the multiplexed Forwarder.
 *
 * Spins up a local echo server (stands in for the target), runs the Forwarder
 * against it over a real WebSocket, and checks that multiple independent
 * sessions share one WebSocket correctly.
 */
import { WebSocketServer, WebSocket } from 'ws';
import net from 'node:net';
import { Readable, Writable } from 'node:stream';
import { Forwarder } from '../src/forward.js';
import { authBytes, bytesToHex } from '../src/crypto.js';
import { buildSessionOpen, buildMuxFrame } from '../src/protocol.js';

const PASSWORD = 'integration-password';
const AUTH = authBytes(PASSWORD);

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };

let targetServer, targetPort;
const upstreamConns = [];

function startEcho() {
	return new Promise((resolve) => {
		targetServer = net.createServer((sock) => {
			upstreamConns.push(sock);
			sock.on('data', (d) => sock.write(Buffer.from('ECHO:' + d.toString())));
			sock.on('error', () => {});
		});
		targetServer.listen(0, '127.0.0.1', () => { targetPort = targetServer.address().port; resolve(); });
	});
}

/** Fake request with a fetcher.connect that opens a local TCP socket. */
function fakeRequest() {
	return {
		fetcher: {
			connect({ port }) {
				const sock = net.connect(port, '127.0.0.1');
				return {
					opened: new Promise((res, rej) => { sock.once('connect', res); sock.once('error', rej); }),
					readable: Readable.toWeb(sock),
					writable: Writable.toWeb(sock),
					closed: new Promise((res) => sock.once('close', res)),
				};
			},
		},
	};
}

async function main() {
	await startEcho();
	console.log(`echo target on 127.0.0.1:${targetPort}\n`);

	// ---------- handshake + single session on a fresh WS ----------
	console.log('--- auth handshake + session ---');
	const wss = new WebSocketServer({ port: 0 });
	await new Promise((r) => wss.once('listening', r));
	const wssPort = wss.address().port;

	wss.on('connection', (ws) => {
		const fwd = new Forwarder(fakeRequest(), ws, AUTH, {
			log: (m) => console.log('   [fwd]', m),
			relays: [], // 本地 echo 不在 443 上，与中继无关；显式置空避免测试触网
		});
		fwd.start();
	});

	const client = new WebSocket(`ws://127.0.0.1:${wssPort}`);
	await new Promise((r, j) => { client.once('open', r); client.once('error', j); });
	const rx = [];
	client.on('message', (d) => rx.push(Buffer.from(d)));

	// wrong auth must be closed with 1008
	console.log('\n--- wrong auth is rejected ---');
	{
		const bad = new WebSocket(`ws://127.0.0.1:${wssPort}`);
		await new Promise((r, j) => { bad.once('open', r); bad.once('error', j); });
		const code = await new Promise((r) => {
			bad.on('close', (c) => r(c));
			bad.send(Buffer.from(AUTH.slice().reverse()));
		});
		ok(code === 1008, 'closed with 1008', `code=${code}`);
	}
	// correct auth, then a session
	{
		const sidA = Uint8Array.from([0xaa]);
		client.send(Buffer.from(AUTH));
		await new Promise((r) => setTimeout(r, 100));
		const open = buildSessionOpen({ host: '127.0.0.1', port: targetPort, payload: Buffer.from('hello') });
		client.send(Buffer.from(buildMuxFrame(sidA, open)));
		await new Promise((r) => setTimeout(r, 500));

		ok(rx.length > 0, 'downstream frame received', `frames=${rx.length}`);
		// The first frame must be the ready signal: [0x00][sidLen][sid] with no
		// payload. The client relies on it to know the session is really up.
		const first = rx[0];
		ok(first[0] === 0, 'first frame is a control (ready) frame', first.slice(0, 4).toString('hex'));
		ok(first.length === 2 + first[1], 'ready frame carries no payload', `len=${first.length}`);
		ok(first.slice(2, 2 + first[1]).toString('hex') === 'aa', 'ready frame names session aa');

		const dataFrames = rx.filter((f) => f[0] === 1);
		ok(dataFrames.length > 0, 'data frames received after ready', `n=${dataFrames.length}`);
		const joined = Buffer.concat(dataFrames);
		ok(joined.toString().includes('ECHO:hello'), 'payload echoed', joined.toString());
	}

	// second session on the SAME websocket
	console.log('\n--- two sessions multiplexed on one WebSocket ---');
	const rxBefore = rx.length;
	const sidB = Uint8Array.from([0xbb]);
	{
		const open = buildSessionOpen({ host: '127.0.0.1', port: targetPort, payload: Buffer.from('world') });
		client.send(Buffer.from(buildMuxFrame(sidB, open)));
		await new Promise((r) => setTimeout(r, 600));
	}
	const newFrames = rx.slice(rxBefore);
	ok(newFrames.length > 0, 'session B got frames', `frames=${newFrames.length}`);
	{
		const joinedB = Buffer.concat(newFrames);
		ok(joinedB.toString().includes('ECHO:world'), 'session B payload echoed', joinedB.toString());
		ok(newFrames.some((f) => f[1] === 0xbb), 'frames tagged 0xbb');
	}
	ok(upstreamConns.length >= 2, 'two separate upstream TCP connections', `conns=${upstreamConns.length}`);

	// ongoing data on session A after B joined (interleaving check)
	const rxBefore2 = rx.length;
	{
		const sidA = Uint8Array.from([0xaa]);
		client.send(Buffer.from(buildMuxFrame(sidA, Buffer.from('again'))));
	}
	await new Promise((r) => setTimeout(r, 400));
	{
		const tail = Buffer.concat(rx.slice(rxBefore2));
		ok(tail.toString().includes('ECHO:again'), 'session A still works after B joined', tail.toString());
		ok(rx.slice(rxBefore2).some((f) => f[1] === 0xaa), 'A frames still tagged 0xaa');
	}

	// the FIRST message must be the auth frame - anything else is unauthorized
	console.log('
--- first message that is not auth is closed as unauthorized ---');
	{
		const c2 = new WebSocket(`ws://127.0.0.1:${wssPort}`);
		await new Promise((r, j) => { c2.once('open', r); c2.once('error', j); });
		const code = await new Promise((r) => {
			c2.on('close', (c) => r(c));
			const sid = Uint8Array.from([0x01]);
			c2.send(Buffer.from(buildMuxFrame(sid, buildSessionOpen({ host: '127.0.0.1', port: targetPort }))));
		});
		ok(code === 1008, 'closed with 1008', `code=${code}`);
	}

	client.close();
	wss.close();
	targetServer.close();
	console.log(`\n${pass} passed, ${fail} failed`);
	process.exit(fail ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
