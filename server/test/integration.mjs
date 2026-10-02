// integration.mjs — 协议 v2 端到端：devserver（Node 同协议对端）+ 本地 echo，
// 覆盖首帧/开帧全部失败码（PRD §4.4）、数据往返、分帧、CLOSE、>64KB 违规。
import net from 'node:net';
import { Buffer } from 'node:buffer';
import WebSocket from 'ws';
import { startDevserver } from './devserver.mjs';
import { authCode } from '../src/crypto.js';
import {
	encodeFirstFrame, encodeOpenFrame, encodeDataFrame, encodeCloseControl,
	MAX_PAYLOAD, STATUS_OK, STATUS_BAD, STATUS_FORBIDDEN, STATUS_NOEXIT,
} from '../src/protocol.js';

let pass = 0, fail = 0;
const ok = (c, n, x = '') => { if (c) { pass++; console.log('  PASS ' + n); } else { fail++; console.log('  FAIL ' + n + ' ' + x); } };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---- 测试用 WS 客户端 ----

function connect(port, { password = 'devserver-password', tsOffset = 0 } = {}) {
	const ws = new WebSocket(`ws://127.0.0.1:${port}/`);
	const client = {
		ws,
		inbox: [],
		waiters: [],
		nextId: 1,
		authed: false,
		closed: null,
		recv(timeoutMs = 5000) {
			if (client.inbox.length) return Promise.resolve(client.inbox.shift());
			return new Promise((res, rej) => {
				const t = setTimeout(() => rej(new Error('recv timeout')), timeoutMs);
				client.waiters.push((msg) => { clearTimeout(t); res(msg); });
			});
		},
		open: () => new Promise((res, rej) => { ws.once('open', res); ws.once('error', rej); }),
		close: () => new Promise((res) => { ws.once('close', res); ws.close(); }),
		// 打开一条流并等响应帧；返回 status。首帧同时完成认证。
		async dial(host, port_, streamId) {
			const id = streamId ?? client.nextId++;
			let frame;
			if (!client.authed) {
				client.authed = true;
				const now = Math.floor(Date.now() / 1000) + tsOffset;
				const stub = encodeFirstFrame(new Uint8Array(16), now, id, host, port_);
				const auth = await authCode(password, stub.slice(16));
				frame = encodeFirstFrame(auth, now, id, host, port_);
			} else {
				frame = encodeOpenFrame(id, host, port_);
			}
			ws.send(frame);
			// 失败响应后服务端会跟一条 CLOSE 控制帧；dial 只关心响应帧，
			// 控制帧留在 inbox 里由下一次 recv 消费。
			for (;;) {
				const resp = await client.recv();
				if (resp.type === 'response' && resp.id === id) return { id, status: resp.status };
			}
		},
		sendData: (id, buf) => ws.send(encodeDataFrame(id, buf)),
		sendClose: (id) => ws.send(encodeCloseControl(id)),
	};
	ws.on('message', (data) => {
		const b = new Uint8Array(data);
		if (process.env.DBG) console.log('   <-', Buffer.from(b).toString('hex').slice(0, 40));
		if (b.length === 5) {
			// 响应帧恒 5 字节；流 ID 0 的响应（0x00000000|status）也与控制帧不冲突，
			// 因为 CLOSE 控制帧最少 9 字节。
			client.inbox.push({ type: 'response', id: (((b[0] << 24) | (b[1] << 16) | (b[2] << 8) | b[3]) >>> 0), status: b[4] });
		} else if (b[0] === 0 && b[1] === 0 && b[2] === 0 && b[3] === 0) {
			client.inbox.push({ control: 'close', id: (((b[5] << 24) | (b[6] << 16) | (b[7] << 8) | b[8]) >>> 0) });
		} else {
			client.inbox.push({ type: 'data', id: (((b[0] << 24) | (b[1] << 16) | (b[2] << 8) | b[3]) >>> 0), payload: b.slice(4) });
		}
		const w = client.waiters.shift();
		if (w) w(client.inbox.shift());
	});
	ws.on('close', (code, reason) => { client.closed = { code, reason: reason.toString() }; });
	return client;
}

// ---- 本地 echo 服务器 ----

function echoServer() {
	const conns = [];
	const srv = net.createServer((sock) => {
		conns.push(sock);
		sock.pipe(sock); // 原样回显
		sock.on('error', () => {});
	});
	return new Promise((res) => srv.listen(0, '127.0.0.1', () => res({ port: srv.address().port, close: () => { for (const c of conns) c.destroy(); srv.close(); } })));
}

// 连接建立后立即推送 size 字节的服务器：测服务端→客户端的分帧与重组。
function bulkServer(size) {
	const srv = net.createServer((sock) => {
		sock.end(Buffer.alloc(size, 0xab));
		sock.on('error', () => {});
	});
	return new Promise((res) => srv.listen(0, '127.0.0.1', () => res({ port: srv.address().port, close: () => srv.close() })));
}

// ---- 套件 ----

const echo = await echoServer();
const bulk = await bulkServer(200 * 1024);
const dev = await startDevserver({ denyLoopback: false, connectTimeoutMs: 800 });
const devLocked = await startDevserver({}); // 默认 denyLoopback：测 0x02

try {
	console.log('--- happy path ---');
	{
		const c = connect(dev.port);
		await c.open();
		const r1 = await c.dial('127.0.0.1', echo.port);
		ok(r1.status === STATUS_OK, 'first frame opens stream 1 (auth ok)');

		// 数据往返
		c.sendData(r1.id, Buffer.from('hello netmaster'));
		const d = await c.recv();
		ok(d.type === 'data' && Buffer.from(d.payload).toString() === 'hello netmaster', 'echo roundtrip');

		// 并发第二条流（开帧路径）
		const r2 = await c.dial('127.0.0.1', echo.port);
		ok(r2.status === STATUS_OK, 'open frame opens stream 2 (no re-auth)');
		c.sendData(r2.id, Buffer.from('second stream'));
		const m2 = await c.recv();
		ok(m2.type === 'data' && m2.id === r2.id && Buffer.from(m2.payload).toString() === 'second stream',
			'second stream echoes independently');

		// 服务端→客户端 200KB：验证按 64KB 分帧后重组
		const big = Buffer.alloc(200 * 1024, 0xab);
		const rb = await c.dial('127.0.0.1', bulk.port);
		ok(rb.status === STATUS_OK, 'bulk stream opens');
		let acc = Buffer.alloc(0);
		while (acc.length < big.length) {
			const m = await c.recv(10000);
			if (m.type === 'data' && m.id === rb.id) acc = Buffer.concat([acc, Buffer.from(m.payload)]);
		}
		ok(acc.length === big.length && acc.equals(big), '200KB payload reassembles');

		// CLOSE：发控制帧后 echo 连接应被服务端关闭
		c.sendClose(r1.id);
		await sleep(150);
		c.ws.close();
		await c.close();
		ok(true, 'close control accepted');
	}

	console.log('--- auth / format failures (0x01) ---');
	{
		const c = connect(dev.port, { password: 'wrong-password' });
		await c.open();
		const r = await c.dial('127.0.0.1', echo.port);
		ok(r.status === STATUS_BAD, 'wrong password -> 0x01');
		await sleep(150);
		ok(c.closed !== null, 'connection closed after auth failure');
	}
	{
		const c = connect(dev.port, { tsOffset: -400 });
		await c.open();
		const r = await c.dial('127.0.0.1', echo.port);
		ok(r.status === STATUS_BAD, 'TS beyond -300s -> 0x01');
		await sleep(150);
		ok(c.closed !== null, 'connection closed after ts rejection');
	}
	{
		const c = connect(dev.port, { tsOffset: 400 });
		await c.open();
		const r = await c.dial('127.0.0.1', echo.port);
		ok(r.status === STATUS_BAD, 'TS beyond +300s -> 0x01');
	}
	{
		// 流 ID 0 的首帧
		const c = connect(dev.port);
		await c.open();
		const now = Math.floor(Date.now() / 1000);
		const stub = encodeFirstFrame(new Uint8Array(16), now, 0, '127.0.0.1', echo.port);
		const auth = await authCode('devserver-password', stub.slice(16));
		c.ws.send(encodeFirstFrame(auth, now, 0, '127.0.0.1', echo.port));
		const r = await c.recv();
		ok(r.status === STATUS_BAD, 'stream id 0 -> 0x01');
	}
	{
		// 已关流上的迟到帧被静默丢弃（不响应、不误解析成开帧）；
		// 流 ID 在回绕到 0xFFFFFFFE 前不复用（PRD §4.5）。
		const c = connect(dev.port);
		await c.open();
		const r1 = await c.dial('127.0.0.1', echo.port, 5);
		ok(r1.status === STATUS_OK, 'stream 5 opens');
		c.sendClose(5);
		await sleep(150);
		let lateArrived = false;
		c.ws.send(encodeOpenFrame(5, '127.0.0.1', echo.port));
		c.ws.once('message', () => { lateArrived = true; });
		await sleep(400);
		ok(!lateArrived, 'late frame on closed stream dropped silently');
		c.ws.close();
	}
	{
		// 0xFFFFFFFF 开帧
		const c = connect(dev.port);
		await c.open();
		await c.dial('127.0.0.1', echo.port, 1);
		c.ws.send(encodeOpenFrame(0xffffffff, '127.0.0.1', echo.port));
		const r = await c.recv();
		ok(r.id === 0xffffffff && r.status === STATUS_BAD, 'stream id 0xFFFFFFFF -> 0x01');
		c.ws.close();
	}
	{
		// 非法 ATYP
		const c = connect(dev.port);
		await c.open();
		await c.dial('127.0.0.1', echo.port, 1);
		c.ws.send(new Uint8Array([0, 0, 0, 9, 0x09, 0x00, 80])); // id=9, atyp=9
		const r = await c.recv();
		ok(r.id === 9 && r.status === STATUS_BAD, 'invalid ATYP -> 0x01');
		c.ws.close();
	}

	console.log('--- forbidden targets (0x02) ---');
	{
		const c = connect(dev.port);
		await c.open();
		const r = await c.dial('10.0.0.1', 80);
		ok(r.status === STATUS_FORBIDDEN, 'private IP -> 0x02');
		const r2 = await c.dial('127.0.0.1', 1); // dev 放开回环：回环端口可拨（通到拒绝）
		ok(r2.status === STATUS_NOEXIT, 'loopback dial allowed when unlocked (refused -> 0x03)');
		const r3 = await c.dial('example.com', 25);
		ok(r3.status === STATUS_FORBIDDEN, 'port 25 -> 0x02');
		const cc = connect(devLocked.port);
		await cc.open();
		const r4 = await cc.dial('127.0.0.1', 12345);
		ok(r4.status === STATUS_FORBIDDEN, 'loopback denied on locked server');
		c.ws.close();
		cc.ws.close();
	}

	console.log('--- exit failure (0x03) ---');
	{
		const c = connect(dev.port);
		await c.open();
		// TEST-NET-1 在服务端保留网段清单里，应被 0x02 拒绝（0x03 路径已由
		// “loopback dial allowed when unlocked”覆盖：连接被拒即 0x03）。
		const r = await c.dial('192.0.2.1', 9);
		ok(r.status === STATUS_FORBIDDEN, 'TEST-NET reserved range -> 0x02', `(got ${r.status})`);
		const r2 = await c.recv();
		ok(r2.control === 'close' && r2.id === r.id, 'rejection followed by CLOSE control');
		c.ws.close();
	}

	console.log('--- oversize payload (protocol violation) ---');
	{
		const c = connect(dev.port);
		await c.open();
		const r = await c.dial('127.0.0.1', echo.port);
		c.sendData(r.id, Buffer.alloc(MAX_PAYLOAD + 1, 1));
		const m = await c.recv();
		ok(m.control === 'close' && m.id === r.id, 'payload > 64KB -> stream closed');
		// 其他流不受影响
		const r2 = await c.dial('127.0.0.1', echo.port);
		ok(r2.status === STATUS_OK, 'other streams unaffected');
		c.ws.close();
	}
} finally {
	dev.close();
	devLocked.close();
	echo.close();
	bulk.close();
}

console.log(`\n${fail === 0 ? 'ALL PASS' : 'FAILURES'}: ${pass} pass, ${fail} fail`);
process.exit(fail === 0 ? 0 : 1);
