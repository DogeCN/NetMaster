// e2e.mjs — 真实部署的端到端验收（PRD §14 M1-M5）。
//
// 两种模式，同一份脚本：
//
//   node test/e2e.mjs
//     本地自检。进程内拉起 devserver.mjs（Node 同协议对端）+ 一个本地 HTTP 目标，
//     跑第 1-4 项证明脚本逻辑本身正确；第 5-8 项如实 SKIP 并写明原因。
//
//   NETMASTER_ENDPOINT=wss://netmaster.<account>.workers.dev/ \
//   NETMASTER_PASSWORD=<worker secret> node test/e2e.mjs
//     真实部署。跑全部 8 项。
//
// 设计约束：
//   · 无外部依赖：只用仓库自带的 ws 与 src/ 下的编解码（与 test/integration.mjs 同源）。
//   · 非交互：任何一项拿不到证据就 SKIP 并写清为什么，绝不输出"看起来像通过"的东西。
//   · 口令只从 NETMASTER_PASSWORD 读，不落盘、不进日志、不接受命令行传入。
//   · 失败即非零退出码（fail>0 → 1；配置缺失 → 2）。
//
// 用法：node test/e2e.mjs [--mode=live|local] [--endpoint=URL] [--rounds=N]
//                         [--idle=SEC] [--streams=N] [--only=1,2,3]

import process from "node:process";
import http from "node:http";
import { WebSocket } from "ws";
import { authCode } from "../src/crypto.js";
import {
  encodeFirstFrame,
  encodeOpenFrame,
  encodeDataFrame,
  encodeCloseControl,
  STATUS_OK,
  STATUS_BAD,
  STATUS_FORBIDDEN,
  STATUS_NOEXIT,
} from "../src/protocol.js";
import { parseRelayEntries, KV_RELAY_KEY } from "../src/race.js";
import { startDevserver } from "./devserver.mjs";

// ---- 小工具 ----

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// be32 读大端 u32：>>> 0 是必须的，否则 0xFFFFFFFF 会变成 -1（Map 键就错位了）。
const be32 = (b, off) => ((b[off] * 0x1000000) + (b[off + 1] << 16) + (b[off + 2] << 8) + b[off + 3]) >>> 0;
const splitTarget = (s) => {
  const i = String(s).lastIndexOf(":");
  return { host: s.slice(0, i), port: Number(s.slice(i + 1)) };
};
const argOf = (name, dflt) => {
  const hit = process.argv.find((a) => a.startsWith(`--${name}=`));
  return hit === undefined ? dflt : hit.slice(name.length + 3);
};
const numEnv = (v, dflt) => {
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : dflt;
};

// normalizeEndpoint 接受裸主机名（netmaster.<account>.workers.dev）或完整 URL。
// 路径固定为 "/"：index.js 只在根路径上做 WS 升级，其余一律 404。
function normalizeEndpoint(raw) {
  const s = String(raw || "").trim();
  if (!s) return "";
  const u = new URL(/^wss?:\/\//i.test(s) ? s : `wss://${s}`);
  u.pathname = "/";
  u.search = "";
  u.hash = "";
  return u.toString();
}

// ---- 配置 ----

const ENDPOINT_RAW = argOf("endpoint", process.env.NETMASTER_ENDPOINT || process.env.NETMASTER_WORKER || "");
const PASSWORD = process.env.NETMASTER_PASSWORD || "";
const MODE = argOf("mode", process.env.E2E_MODE || "") || (ENDPOINT_RAW ? "live" : "local");
const ONLY = argOf("only", process.env.E2E_ONLY || "")
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean);

if (MODE !== "live" && MODE !== "local") {
  console.error(`e2e: unknown --mode=${MODE} (want live|local)`);
  process.exit(2);
}
if (MODE === "live" && !PASSWORD) {
  console.error("e2e: NETMASTER_PASSWORD is required in live mode (the Worker secret; never passed on the command line)");
  process.exit(2);
}

const CF = {
  apiBase: "https://api.cloudflare.com/client/v4",
  token: process.env.CLOUDFLARE_API_TOKEN || "",
  account: process.env.CLOUDFLARE_ACCOUNT_ID || "",
  kvId: process.env.NETMASTER_KV_ID || "",
};

const cfg = {
  mode: MODE,
  endpoint: MODE === "live" ? normalizeEndpoint(ENDPOINT_RAW) : "",
  password: PASSWORD,
  // 直连出口的目标：明文 HTTP 且非 CF 托管（走 connect() 直连路径）。
  direct: splitTarget(argOf("direct", process.env.E2E_DIRECT_TARGET || "example.com:80")),
  // CF 托管的明文 HTTP 目标：直连会被平台拒，只能走 ProxyIP 竞速。
  cf: splitTarget(argOf("cf", process.env.E2E_CF_TARGET || "neverssl.com:80")),
  // 端口 25 的目标用固定域名而不是本地目标：devserver 在 denyLoopback=false 时对
  // 127.0.0.1 整条跳过禁连检查（那是测试专用开关），而生产是 exits.js 里
  // port===25 最先判 —— 用域名才能验到同一条路径。
  smtp: splitTarget(argOf("smtp", process.env.E2E_SMTP_TARGET || "example.com:25")),
  streams: numEnv(argOf("streams", process.env.E2E_MUX_STREAMS), 100),
  rounds: numEnv(argOf("rounds", process.env.E2E_CF_ROUNDS), 20),
  minRate: Number(argOf("minrate", process.env.E2E_CF_MIN_RATE) || 99),
  idleSec: numEnv(argOf("idle", process.env.E2E_IDLE_SECONDS), MODE === "live" ? 300 : 12),
  pingSec: numEnv(argOf("ping", process.env.E2E_PING_SECONDS), MODE === "live" ? 30 : 3),
  streamTimeoutMs: numEnv(process.env.E2E_STREAM_TIMEOUT_MS, 25000),
};

// ---- 结果收集 ----

const results = [];
function record(name, status, detail) {
  results.push({ name, status, detail });
  console.log(`${status.padEnd(4)} ${name}${detail ? ` — ${detail}` : ""}`);
}
const pass = (detail) => ({ ok: true, detail });
const fail = (detail) => ({ ok: false, detail });
const skip = (reason) => ({ skipped: true, reason });
const note = (s) => console.log(`       ${s}`);

async function step(name, fn) {
  if (ONLY.length && !ONLY.some((n) => name.startsWith(n))) return;
  try {
    const r = await fn();
    if (r.skipped) record(name, "SKIP", r.reason);
    else record(name, r.ok ? "PASS" : "FAIL", r.detail);
  } catch (e) {
    record(name, "FAIL", `threw: ${e.message || e}`);
  }
}

// ---- 协议 v2 客户端 ----

// 每条流的接收状态。data/chunks 累积数据帧，statusW/closeW 是等待者队列——
// 帧可能比等待者先到，所以状态存在 rec 上而不是只放在 promise 里。
class Mux {
  constructor(url, password) {
    this.url = url;
    this.password = password;
    this.authed = false;
    this.nextId = 1;
    this.streams = new Map();
    this.closeEvent = null;
    this.pings = 0;
    this.pongs = 0;
    this.rx = 0;
    this.tx = 0;
    this.error = null;
    this.ws = new WebSocket(url, { handshakeTimeout: 20000 });
    this.ws.on("message", (data, isBinary) => this.onMessage(data, isBinary));
    this.ws.on("close", (code, reason) => {
      this.closeEvent = { code, reason: String(reason || ""), at: Date.now() };
    });
    this.ws.on("error", (e) => {
      this.error = e.message || String(e);
    });
    this.ws.on("ping", () => {
      try {
        this.ws.pong();
      } catch {}
    });
    // Pong 由边缘自动应答，脚本自己数：M2 的判活证据就是"ping 有多少、pong 回多少"。
    this.ws.on("pong", () => {
      this.pongs++;
    });
  }

  onMessage(data, isBinary) {
    if (!isBinary) return;
    const b = data instanceof ArrayBuffer ? new Uint8Array(data) : new Uint8Array(data);
    this.rx++;
    // 响应帧恒 5 字节；CLOSE 控制帧 ≥ 9 字节且前 4 字节全 0；其余是数据帧。
    if (b.length === 5) {
      const rec = this.rec(be32(b, 0));
      rec.status = b[4];
      rec.statusW.splice(0).forEach((w) => w(rec.status));
      return;
    }
    if (b.length >= 9 && b[0] === 0 && b[1] === 0 && b[2] === 0 && b[3] === 0) {
      const rec = this.rec(be32(b, 5));
      rec.closed = true;
      rec.closeW.splice(0).forEach((w) => w(true));
      return;
    }
    if (b.length >= 4) {
      const rec = this.rec(be32(b, 0));
      const payload = b.subarray(4);
      rec.bytes += payload.byteLength;
      // 响应体上限 256 KiB：验收只看状态码与非空 body，不必无限收。
      if (rec.bytes <= 256 * 1024) rec.chunks.push(Buffer.from(payload));
      else rec.truncated = true;
    }
  }

  rec(id) {
    let r = this.streams.get(id);
    if (!r) {
      r = { id, status: null, closed: false, bytes: 0, chunks: [], truncated: false, statusW: [], closeW: [] };
      this.streams.set(id, r);
    }
    return r;
  }

  open(timeoutMs = 20000) {
    if (this.ws.readyState === WebSocket.OPEN) return Promise.resolve();
    return new Promise((res, rej) => {
      const t = setTimeout(() => {
        done();
        rej(new Error(`ws open timeout after ${timeoutMs}ms${this.error ? `: ${this.error}` : ""}`));
      }, timeoutMs);
      const onOpen = () => {
        done();
        res();
      };
      const onErr = (e) => {
        done();
        rej(new Error(`ws error: ${e.message || e}`));
      };
      const onClose = (code) => {
        done();
        rej(new Error(`ws closed during handshake (code ${code})`));
      };
      const done = () => {
        clearTimeout(t);
        this.ws.off("open", onOpen);
        this.ws.off("error", onErr);
        this.ws.off("close", onClose);
      };
      this.ws.on("open", onOpen);
      this.ws.on("error", onErr);
      this.ws.on("close", onClose);
    });
  }

  waitStatus(id, timeoutMs = 20000) {
    const r = this.rec(id);
    if (r.status !== null) return Promise.resolve(r.status);
    return new Promise((res, rej) => {
      const t = setTimeout(() => rej(new Error(`no STATUS frame for stream ${id} within ${timeoutMs}ms`)), timeoutMs);
      r.statusW.push((s) => {
        clearTimeout(t);
        res(s);
      });
    });
  }

  // waitClose 等 CLOSE 控制帧；返回 false 表示超时（不是"流关了"，是"证据没到"）。
  waitClose(id, timeoutMs = 20000) {
    const r = this.rec(id);
    if (r.closed) return Promise.resolve(true);
    return new Promise((res) => {
      const t = setTimeout(() => res(false), timeoutMs);
      r.closeW.push((v) => {
        clearTimeout(t);
        res(v);
      });
    });
  }

  waitSocketClose(timeoutMs = 3000) {
    if (this.closeEvent) return Promise.resolve(true);
    return new Promise((res) => {
      const t = setTimeout(() => res(false), timeoutMs);
      this.ws.once("close", () => {
        clearTimeout(t);
        res(true);
      });
    });
  }

  // firstFrame 构造首帧：签名区 = AUTH 之后的全部字节（TS‖ID‖ATYP‖ADDR‖PORT）。
  async firstFrame(id, target, { tsOffsetSec = 0, password } = {}) {
    const ts = Math.floor(Date.now() / 1000) + tsOffsetSec;
    const stub = encodeFirstFrame(new Uint8Array(16), ts, id, target.host, target.port);
    if (!stub) throw new Error(`cannot encode first frame for ${target.host}:${target.port}`);
    const auth = await authCode(password ?? this.password, stub.slice(16));
    return encodeFirstFrame(auth, ts, id, target.host, target.port);
  }

  sendOpen(id, target) {
    const f = encodeOpenFrame(id, target.host, target.port);
    if (!f) throw new Error(`cannot encode open frame for ${target.host}:${target.port}`);
    this.tx++;
    this.ws.send(f);
    return this.rec(id);
  }

  // dial 开一条流并等响应帧。首帧（带认证）只在连接上的第一次调用里发。
  async dial(target, opts = {}) {
    const id = opts.id ?? this.nextId++;
    if (id >= this.nextId) this.nextId = id + 1;
    const t0 = Date.now();
    if (!this.authed) {
      this.authed = true;
      this.tx++;
      this.ws.send(await this.firstFrame(id, target, opts));
    } else {
      this.sendOpen(id, target);
    }
    const status = await this.waitStatus(id, opts.timeoutMs ?? cfg.streamTimeoutMs);
    return { id, status, ms: Date.now() - t0, rec: this.rec(id) };
  }

  sendData(id, payload) {
    this.tx++;
    this.ws.send(encodeDataFrame(id, payload));
  }

  sendClose(id) {
    this.tx++;
    this.ws.send(encodeCloseControl(id));
  }

  ping() {
    this.pings++;
    try {
      this.ws.ping();
    } catch {}
  }

  async close() {
    try {
      this.ws.close();
    } catch {}
    await sleep(50);
  }
}

// httpGet 在一条流上发一次 HTTP/1.1 GET 并收完整响应。Connection: close 让目标
// 端关闭连接，服务端随即发 CLOSE 控制帧——WS 保序，所以 CLOSE 到达时 body 已收全。
async function httpGet(mux, rec, target, path = "/", timeoutMs = 20000) {
  const req = `GET ${path} HTTP/1.1\r\nHost: ${target.host}\r\nUser-Agent: netmaster-e2e\r\nAccept: */*\r\nConnection: close\r\n\r\n`;
  const t0 = Date.now();
  mux.sendData(rec.id, Buffer.from(req, "latin1"));
  const closed = await mux.waitClose(rec.id, timeoutMs);
  const text = Buffer.concat(rec.chunks).toString("latin1");
  const i = text.indexOf("\r\n\r\n");
  const head = i >= 0 ? text.slice(0, i) : text;
  const body = i >= 0 ? text.slice(i + 4) : "";
  const m = /^HTTP\/1\.[01] (\d{3})/.exec(head);
  return {
    code: m ? Number(m[1]) : 0,
    bodyBytes: Buffer.byteLength(body, "latin1"),
    headerEnd: i >= 0,
    closed,
    truncated: rec.truncated,
    ms: Date.now() - t0,
  };
}

const statusName = (s) => `0x0${s}`;

// ---- 本地模式：进程内 devserver + 本地 HTTP 目标 ----

// 本地 HTTP 目标：固定 200 + body 标记，100 条并发流也不会把它压垮。
function localHttpTarget(marker) {
  const body = Buffer.from(`${marker}\n`, "utf8");
  return new Promise((resolve) => {
    const srv = http.createServer((req, res) => {
      res.writeHead(200, {
        "content-type": "text/plain",
        "content-length": String(body.length),
        connection: "close",
      });
      res.end(body);
    });
    srv.on("connection", (s) => s.on("error", () => {}));
    srv.listen(0, "127.0.0.1", () => {
      resolve({
        target: { host: "127.0.0.1", port: srv.address().port },
        close: () => srv.close(),
      });
    });
  });
}

// ---- Cloudflare REST（KV 快照） ----

async function cfApi(path) {
  const r = await fetch(`${CF.apiBase}${path}`, { headers: { authorization: `Bearer ${CF.token}` } });
  return { status: r.status, text: await r.text() };
}

// resolveKvNamespace 按 deploy.yml 的口径解析命名空间 id（title === "netmaster"），
// NETMASTER_KV_ID 可以直接钉死。查不到就返回 error——不猜。
async function resolveKvNamespace() {
  if (CF.kvId) return { id: CF.kvId, how: "NETMASTER_KV_ID" };
  const r = await cfApi(`/accounts/${CF.account}/storage/kv/namespaces`);
  if (r.status !== 200) return { error: `namespace list HTTP ${r.status}: ${r.text.slice(0, 200)}` };
  let list = [];
  try {
    list = JSON.parse(r.text).result || [];
  } catch (e) {
    return { error: `namespace list is not JSON: ${e.message}` };
  }
  const ns = list.find((x) => x.title === "netmaster") || list[0];
  return ns ? { id: ns.id, how: `title=${ns.title}` } : { error: "no KV namespace in this account" };
}

async function kvGet(ns, key) {
  const r = await cfApi(`/accounts/${CF.account}/storage/kv/namespaces/${ns}/values/${encodeURIComponent(key)}`);
  if (r.status === 404) return { missing: true };
  if (r.status !== 200) return { error: `HTTP ${r.status}: ${r.text.slice(0, 200)}` };
  return { value: r.text };
}

// ---- 验收项 ----

// 1 鉴权与协议（PRD §4.4/§5）：每个子项都要"响应帧 + 连接关闭"两个证据。
async function item1() {
  const T = cfg.direct;
  await step("1.1 wrong password -> 0x01 + close", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const r = await m.dial(T, { password: `${cfg.password}-wrong` });
    if (r.status !== STATUS_BAD) {
      await m.close();
      return fail(`status ${statusName(r.status)}, want ${statusName(STATUS_BAD)}`);
    }
    const closed = await m.waitSocketClose(3000);
    await m.close();
    return closed
      ? pass(`0x01 then close (code ${m.closeEvent?.code})`)
      : fail("0x01 received but connection stayed open");
  });

  await step("1.2 TS -400s (beyond -300s window) -> 0x01 + close", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const r = await m.dial(T, { tsOffsetSec: -400 });
    if (r.status !== STATUS_BAD) {
      await m.close();
      return fail(`status ${statusName(r.status)}, want ${statusName(STATUS_BAD)}`);
    }
    const closed = await m.waitSocketClose(3000);
    await m.close();
    return closed ? pass("0x01 then close") : fail("0x01 received but connection stayed open");
  });

  await step("1.3 TS +400s (beyond +300s window) -> 0x01 + close", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const r = await m.dial(T, { tsOffsetSec: 400 });
    if (r.status !== STATUS_BAD) {
      await m.close();
      return fail(`status ${statusName(r.status)}, want ${statusName(STATUS_BAD)}`);
    }
    const closed = await m.waitSocketClose(3000);
    await m.close();
    return closed ? pass("0x01 then close") : fail("0x01 received but connection stayed open");
  });

  await step("1.4 STREAM_ID=0 in first frame -> 0x01 + close", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    m.authed = true;
    m.tx++;
    m.ws.send(await m.firstFrame(0, T)); // 签名是对的，只是流 ID 非法
    const status = await m.waitStatus(0, 8000);
    if (status !== STATUS_BAD) {
      await m.close();
      return fail(`status ${statusName(status)}, want ${statusName(STATUS_BAD)}`);
    }
    const closed = await m.waitSocketClose(3000);
    await m.close();
    return closed ? pass("0x01 (id=0) then close") : fail("0x01 received but connection stayed open");
  });

  await step("1.5 STREAM_ID=0xFFFFFFFF in open frame -> 0x01", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const first = await m.dial(T, { id: 1 }); // 首帧顺带完成认证
    if (first.status !== STATUS_OK) {
      await m.close();
      return fail(`baseline stream did not open (status ${statusName(first.status)})`);
    }
    m.sendOpen(0xffffffff, T);
    const status = await m.waitStatus(0xffffffff, 8000);
    await m.close();
    return status === STATUS_BAD
      ? pass("0x01 for the reserved id 0xFFFFFFFF")
      : fail(`status ${statusName(status)}, want ${statusName(STATUS_BAD)}`);
  });

  await step("1.6 invalid ATYP=9 in open frame -> 0x01", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const first = await m.dial(T, { id: 1 });
    if (first.status !== STATUS_OK) {
      await m.close();
      return fail(`baseline stream did not open (status ${statusName(first.status)})`);
    }
    m.tx++;
    m.ws.send(Uint8Array.of(0, 0, 0, 9, 9, 0, 80)); // id=9, atyp=9（不存在）
    const status = await m.waitStatus(9, 8000);
    await m.close();
    return status === STATUS_BAD
      ? pass("0x01 for ATYP 9")
      : fail(`status ${statusName(status)}, want ${statusName(STATUS_BAD)}`);
  });
  note("first frame with an illegal ATYP is rejected structurally: session.js only replies");
  note("when parseFirstFrame succeeds, so that case yields close-without-STATUS (covered by 1.6 on the open-frame path)");
}

// 2 禁连目标 → 0x02 + 紧随其后的 CLOSE 控制帧（PRD §4.4）。
async function item2() {
  const cases = [
    { label: "2.1 private IP 10.0.0.1:80 -> 0x02 + CLOSE", target: { host: "10.0.0.1", port: 80 } },
    { label: "2.2 TEST-NET-1 192.0.2.1:80 -> 0x02 + CLOSE", target: { host: "192.0.2.1", port: 80 } },
    { label: "2.3 port 25 -> 0x02 + CLOSE", target: cfg.smtp },
  ];
  for (const c of cases) {
    await step(c.label, async () => {
      const m = new Mux(cfg.endpoint, cfg.password);
      await m.open();
      const r = await m.dial(c.target, { timeoutMs: 10000 });
      if (r.status !== STATUS_FORBIDDEN) {
        await m.close();
        return fail(`status ${statusName(r.status)}, want ${statusName(STATUS_FORBIDDEN)}`);
      }
      const closed = await m.waitClose(r.id, 5000);
      await m.close();
      return closed
        ? pass(`${statusName(STATUS_FORBIDDEN)} + CLOSE for stream ${r.id}`)
        : fail(`${statusName(STATUS_FORBIDDEN)} but no CLOSE control frame within 5s`);
    });
  }
}

// 3 直连出口成功：证明整条链路 + Session DO + connect() 出站都活着（M1）。
async function item3() {
  await step("3.1 direct exit: stream 0x00 + HTTP 200 + body", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const r = await m.dial(cfg.direct, { timeoutMs: 30000 });
    if (r.status !== STATUS_OK) {
      await m.close();
      return fail(`status ${statusName(r.status)}, want 0x00 (direct connect to ${cfg.direct.host}:${cfg.direct.port} failed)`);
    }
    const resp = await httpGet(m, r.rec, cfg.direct, "/", 20000);
    await m.close();
    if (!resp.headerEnd) return fail(`no HTTP status line in ${resp.bytes === 0 ? 0 : r.rec.bytes} bytes (stream open took ${r.ms}ms)`);
    const ok = resp.code === 200 && resp.bodyBytes > 0;
    return ok
      ? pass(`HTTP ${resp.code}, body ${resp.bodyBytes}B, open ${r.ms}ms, request ${resp.ms}ms via ${cfg.direct.host}:${cfg.direct.port}`)
      : fail(`HTTP ${resp.code}, body ${resp.bodyBytes}B (want 200 with a non-empty body)${resp.closed ? "" : ", response never completed"}`);
  });
}

// 4 多路复用：一条 WS 上并发开 ≥100 条流（M2 的量化项）。
async function item4() {
  const n = cfg.streams;
  await step(`4.1 mux: ${n} concurrent streams on one ws, each completing a request`, async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    // 首帧先建一条流完成认证，之后 n-1 条开帧一次性全部发出去——这才是"并发"
    // 而不是"串行建流"，服务端会同时握手上百个出站 socket。
    const ids = [];
    const t0 = Date.now();
    const first = await m.dial(cfg.direct, { timeoutMs: 30000 });
    if (first.status !== STATUS_OK) {
      await m.close();
      return fail(`first stream did not open (status ${statusName(first.status)})`);
    }
    ids.push(first.id);
    for (let i = 1; i < n; i++) {
      const id = m.nextId++;
      m.sendOpen(id, cfg.direct);
      ids.push(id);
    }
    const statuses = await Promise.all(
      ids.slice(1).map((id) => m.waitStatus(id, 60000).catch(() => -1))
    );
    const openMs = Date.now() - t0;
    const opened = 1 + statuses.filter((s) => s === STATUS_OK).length;
    const byStatus = new Map();
    for (const s of statuses) byStatus.set(s, (byStatus.get(s) || 0) + 1);
    note(`opened ${opened}/${n} in ${openMs}ms; statuses: ${[...byStatus].map(([s, c]) => `${s === -1 ? "timeout" : statusName(s)}x${c}`).join(" ")}`);
    if (opened < n) {
      await m.close();
      return fail(`only ${opened}/${n} streams got 0x00 (${opened} < ${n})`);
    }
    // 每条流各自完成一次请求。
    const results = await Promise.all(ids.map((id) => httpGet(m, m.rec(id), cfg.direct, "/", 30000)));
    const good = results.filter((x) => x.code === 200 && x.bodyBytes > 0).length;
    const worst = Math.max(...results.map((x) => x.ms));
    await m.close();
    note(`requests: ${good}/${n} returned HTTP 200 with body, slowest ${worst}ms`);
    return good === n
      ? pass(`${opened}/${n} streams 0x00, ${good}/${n} requests HTTP 200 (ws rx ${m.rx} frames / tx ${m.tx})`)
      : fail(`${opened}/${n} opened but only ${good}/${n} requests returned HTTP 200`);
  });
}

// 5 ProxyIP 出口（M3 的 ≥99%）。失败必须分清三种原因：0x03 全竞速失败 /
// 隧道通了但出口被拒（403）/ 超时。
async function item5() {
  if (cfg.mode !== "live") {
    return step("5.1 ProxyIP exit to CF-hosted target: N-round success rate", async () =>
      skip("local mode: devserver has no ProxyIP/RACING path, and CF-hosted targets are unreachable from this network")
    );
  }
  await step("5.1 ProxyIP exit to CF-hosted target: N-round success rate", async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    let ok = 0;
    const reasons = new Map();
    const bump = (k) => reasons.set(k, (reasons.get(k) || 0) + 1);
    const lat = [];
    for (let i = 0; i < cfg.rounds; i++) {
      let r;
      try {
        r = await m.dial(cfg.cf, { timeoutMs: cfg.streamTimeoutMs });
      } catch (e) {
        bump(`open timeout: ${e.message.replace(/\(.*\)/, "").trim()}`);
        continue;
      }
      if (r.status === STATUS_NOEXIT) {
        bump("all race slots failed (STATUS 0x03)");
        continue;
      }
      if (r.status !== STATUS_OK) {
        bump(`stream status ${statusName(r.status)}`);
        continue;
      }
      let resp;
      try {
        resp = await httpGet(m, r.rec, cfg.cf, "/", 20000);
      } catch (e) {
        bump(`request failed: ${e.message}`);
        continue;
      }
      lat.push(r.ms);
      if (!resp.headerEnd) {
        bump(`no HTTP response (${resp.closed ? "closed early" : "timeout"})`);
        continue;
      }
      if (resp.code === 200 || (resp.code >= 200 && resp.code < 400)) ok++;
      else if (resp.code === 403 || resp.code === 401) bump(`tunnel ok, egress rejected: HTTP ${resp.code}`);
      else bump(`HTTP ${resp.code}`);
      m.sendClose(r.id);
      await sleep(300);
    }
    await m.close();
    const rate = (ok / cfg.rounds) * 100;
    note(`target ${cfg.cf.host}:${cfg.cf.port} (CF-hosted → ProxyIP race), ${cfg.rounds} rounds`);
    note(`stream-open latency: ${lat.length ? `min ${Math.min(...lat)}ms / median ${median(lat)}ms / max ${Math.max(...lat)}ms` : "no successful opens"}`);
    for (const [r, c] of reasons) note(`failure x${c}: ${r}`);
    return rate >= cfg.minRate
      ? pass(`${ok}/${cfg.rounds} = ${rate.toFixed(1)}% (target ≥${cfg.minRate}%)`)
      : fail(`${ok}/${cfg.rounds} = ${rate.toFixed(1)}% < ${cfg.minRate}%`);
  });
}

function median(a) {
  const s = [...a].sort((x, y) => x - y);
  return s.length % 2 ? s[(s.length - 1) / 2] : Math.round((s[s.length / 2 - 1] + s[s.length / 2]) / 2);
}

// 6 Router DO 生效：跨连接复用同一目标的映射。必须每轮换新连接——同一连接里
// 第 2/3 轮命中的是 Session DO 的会话级内存缓存（session.js 的 this.egress），
// 那样证明不了 Router DO。每轮之间等 6s，让 Router DO 的 +5s Alarm flush 落盘。
async function item6() {
  if (cfg.mode !== "live") {
    return step("6.1 router do: 3 sequential connects to the same CF-hosted target", async () =>
      skip("local mode: devserver has no ROUTER binding")
    );
  }
  await step("6.1 router do: 3 sequential connects to the same CF-hosted target", async () => {
    const rows = [];
    for (let i = 1; i <= 3; i++) {
      const m = new Mux(cfg.endpoint, cfg.password);
      await m.open();
      const t0 = Date.now();
      let row = { round: i, status: null, error: null, openMs: Date.now() - t0, http: null, totalMs: Date.now() - t0 };
      try {
        const r = await m.dial(cfg.cf, { timeoutMs: cfg.streamTimeoutMs });
        const resp = r.status === STATUS_OK ? await httpGet(m, r.rec, cfg.cf, "/", 20000) : null;
        row = {
          round: i,
          status: r.status,
          error: null,
          openMs: Date.now() - t0,
          http: resp ? resp.code : null,
          totalMs: Date.now() - t0,
        };
      } catch (e) {
        row = { round: i, status: null, error: e.message, openMs: Date.now() - t0, http: null, totalMs: Date.now() - t0 };
      }
      await m.close();
      rows.push(row);
      note(
        `round ${i}: ${row.error ? `error: ${row.error}` : `status ${statusName(row.status)}`}, ` +
          `open ${row.openMs}ms, total ${row.totalMs}ms${row.http ? `, HTTP ${row.http}` : ""}`
      );
      if (i < 3) await sleep(6000); // 等 Router DO Alarm flush（FLUSH_DELAY_MS = 5000）
    }
    note("Router DO storage is not readable through any public Cloudflare API, so this is a");
    note("black-box observation (no 0x03 from round 2 on + latency), not proof of a DO hit.");
    const bad = rows.filter((r, i) => i > 0 && r.status !== STATUS_OK);
    if (bad.length) return fail(`round(s) ${bad.map((r) => r.round).join(", ")} did not get 0x00 → cached route not reused`);
    return pass(`round 1 ${statusName(rows[0].status)} (${rows[0].openMs}ms), rounds 2-3 ${statusName(STATUS_OK)} (${rows[1].openMs}ms / ${rows[2].openMs}ms) with no 0x03`);
  });
}

// 7 KV / 中继池：读 proxyip:top。
//
// 这个 key 由 GitHub Actions 的 refresh-relays 定时任务写（Worker Cron 已移除），
// 而那个定时任务**默认是关的**：KV 为空不代表部署坏了，只代表"还没启用刷新或还没跑过"。
// 所以空 = SKIP 并写清怎么让它有值，而不是 FAIL —— 否则一个全新部署会红着一项，
// 而红的原因与"这个 Worker 能不能用"毫无关系。
//
// 有数据时照旧校验格式（喂给 race.js 的 parseRelayEntries 能读回可用中继）。
// 不再读 cron:lastRun：那个键随 Worker Cron 一起没了，refresher 只写 proxyip:top
// 一个键；新鲜度只能去 Actions 的 run 页面看（这里如实说明，不伪造证据）。
async function item7() {
  await step("7.1 relay pool in KV (proxyip:top)", async () => {
    if (!CF.token || !CF.account) {
      return skip("CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID not set (KV is only readable through the Cloudflare REST API)");
    }
    const ns = await resolveKvNamespace();
    if (ns.error) return fail(ns.error);
    const top = await kvGet(ns.id, KV_RELAY_KEY);
    note(`namespace ${ns.id} (${ns.how})`);
    const show = (k, v) => (v.missing ? `${k} = (missing)` : v.error ? `${k} = ERROR ${v.error}` : `${k} = ${v.value.trim().slice(0, 300)}`);
    note(show(KV_RELAY_KEY, top));
    if (top.error) return fail("KV read failed");

    if (top.missing || !String(top.value).trim()) {
      return skip("subscription refresh workflow has not run; enable it to populate proxyip:top");
    }
    const relays = parseRelayEntries(top.value);
    if (!relays.length) return fail(`proxyip:top present but yields 0 usable relays: ${top.value.trim().slice(0, 120)}`);
    note("freshness: no lastRun key — the refresher writes only proxyip:top; check the Actions run page for when it last ran");
    return pass(`${relays.length} relay(s): ${relays.map((r) => `${r.host}:${r.port}`).join(", ")}`);
  });
}

// 8 空闲存活：一条已认证的会话，只走 WS 协议层 Ping，静置 cfg.idleSec 秒。
async function item8() {
  await step(`8.1 idle survival: protocol-level ping only, ${cfg.idleSec}s`, async () => {
    const m = new Mux(cfg.endpoint, cfg.password);
    await m.open();
    const auth = await m.dial(cfg.direct, { timeoutMs: 30000 });
    if (auth.status !== STATUS_OK) {
      await m.close();
      return fail(`could not establish an authenticated session (status ${statusName(auth.status)})`);
    }
    m.sendClose(auth.id); // 会话保留（认证是连接级的），但不留挂着的流
    await sleep(500);
    const t0 = Date.now();
    let ticks = 0;
    let lost = null;
    while (Date.now() - t0 < cfg.idleSec * 1000) {
      await sleep(cfg.pingSec * 1000);
      ticks++;
      if (m.closeEvent) {
        lost = { at: Math.round((m.closeEvent.at - t0) / 1000), code: m.closeEvent.code };
        break;
      }
      m.ping();
      await sleep(200); // 给 Pong 一点时间，否则 alive 只是"没看到 close"
      note(
        `t+${Math.round((Date.now() - t0) / 1000)}s alive=${!m.closeEvent} pings=${m.pings} pongs=${m.pongs} ` +
          `readyState=${m.ws.readyState} (OPEN=${WebSocket.OPEN})`
      );
    }
    const elapsed = Math.round((Date.now() - t0) / 1000);
    if (lost) {
      await m.close();
      return fail(`connection closed ${lost.at}s into the idle window (code ${lost.code}) — session.js closes idle sessions at ~185s and WS protocol pings do not reset that timer`);
    }
    await m.close();
    const expectPongs = Math.max(1, ticks - 1);
    if (m.pongs < expectPongs) return fail(`only ${m.pongs}/${expectPongs} expected pongs in ${elapsed}s`);
    if (cfg.mode !== "live") {
      return pass(`local: ${elapsed}s survived, ${m.pings} pings / ${m.pongs} pongs — proves the ping/pong loop, NOT the 5-minute requirement (devserver has no idle timer)`);
    }
    return pass(`${elapsed}s idle survived, ${m.pings} pings / ${m.pongs} pongs, no close event`);
  });
}

// ---- main ----

console.log(`NetMaster e2e — mode=${cfg.mode} endpoint=${cfg.endpoint || "(in-process devserver)"} node=${process.version}`);
if (cfg.mode === "local") {
  console.log("LOCAL SELF-CHECK: items 1-4 run against devserver.mjs; 5-8 are SKIPped. This is not deployment acceptance.");
} else {
  console.log(`direct target ${cfg.direct.host}:${cfg.direct.port} | CF-hosted target ${cfg.cf.host}:${cfg.cf.port}`);
}
if (ONLY.length) console.log(`filter: only ${ONLY.join(", ")}`);

let local = null;
if (cfg.mode === "local") {
  const http1 = await localHttpTarget("netmaster-e2e-local");
  // denyLoopback: false —— 生产禁回环，本地验收必须能拨 127.0.0.1 的 HTTP 目标。
  const dev = await startDevserver({ password: "e2e-local-password", denyLoopback: false, connectTimeoutMs: 2000 });
  cfg.endpoint = dev.url;
  cfg.password = "e2e-local-password";
  cfg.direct = http1.target;
  local = { dev, http1 };
}

try {
  await item1();
  await item2();
  await item3();
  await item4();
  await item5();
  await item6();
  await item7();
  await item8();
} finally {
  if (local) {
    local.dev.close();
    local.http1.close();
  }
}

const nPass = results.filter((r) => r.status === "PASS").length;
const nFail = results.filter((r) => r.status === "FAIL").length;
const nSkip = results.filter((r) => r.status === "SKIP").length;
console.log(`E2E: ${nPass} pass, ${nFail} fail, ${nSkip} skip`);
process.exit(nFail > 0 ? 1 : 0);