/**
 * devserver.mjs — run the real Forwarder over a local WebSocket server so the
 * Go client can be tested against it end to end.
 *
 * Usage: node test/devserver.mjs [port] [password]
 */
import { WebSocketServer } from 'ws';
import net from 'node:net';
import { Readable, Writable } from 'node:stream';
import { Forwarder } from '../src/forward.js';
import { authBytes } from '../src/crypto.js';

const PORT = Number(process.argv[2] || 8799);
const PASSWORD = process.argv[3] || 'devserver-password';
const AUTH = authBytes(PASSWORD);

// A couple of local targets so the client has something real to fetch.
const TARGET_PORT = 8800;
net.createServer((sock) => {
	sock.on('error', () => {});
	sock.on('data', () => {
		const body = JSON.stringify({ ok: true, via: 'devserver', host: '127.0.0.1' });
		sock.write(
			'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: ' +
			body.length + '\r\nConnection: close\r\n\r\n' + body
		);
	});
}).listen(TARGET_PORT, '127.0.0.1', () => console.log(`[dev] target http server on ${TARGET_PORT}`));

const fakeRequest = {
	fetcher: {
		connect({ hostname, port }) {
			const sock = net.connect(port, hostname === 'localhost' ? '127.0.0.1' : hostname);
			return {
				opened: new Promise((res, rej) => { sock.once('connect', res); sock.once('error', rej); }),
				readable: Readable.toWeb(sock),
				writable: Writable.toWeb(sock),
				closed: new Promise((r) => sock.once('close', r)),
			};
		},
	},
};

const wss = new WebSocketServer({ port: PORT });
wss.on('connection', (ws) => {
	const fwd = new Forwarder(fakeRequest, ws, AUTH, {
		log: (m) => console.log('  [fwd]', m),
	});
	fwd.start();
	console.log('[dev] ws connection');
});

console.log(`[dev] ws on ws://127.0.0.1:${PORT}`);
console.log(`[dev] password=${PASSWORD}`);
console.log(`[dev] auth=${Buffer.from(AUTH).toString('hex')}`);
console.log(`[dev] test target port = ${TARGET_PORT}`);
