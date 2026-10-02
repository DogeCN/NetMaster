// M0 探针驱动：按 E1-E4 顺序执行实验并汇总输出。
// 用法：node drive.mjs <worker-base-url> <probe-token>
// 依赖 m0/node_modules 里的 ws。产出 drive-results.json。

import WebSocket from "ws";
import { readFileSync, writeFileSync } from "node:fs";
import { HttpsProxyAgent } from "https-proxy-agent";

const PROXY = process.env.HTTPS_PROXY || process.env.HTTP_PROXY || null;

const BASE = (process.argv[2] || "").replace(/\/$/, "");
const TOKEN = process.argv[3];
if (!BASE || !TOKEN) {
  console.error("usage: node drive.mjs <base-url> <probe-token>");
  process.exit(2);
}

const out = [];
function report(section, data) {
  const line = { section, ...data };
  out.push(line);
  console.log("== " + section + " ==");
  console.log(JSON.stringify(line, null, 2));
}

async function http(path) {
  const r = await fetch(BASE + path + (path.includes("?") ? "&" : "?") + "t=" + TOKEN);
  const text = await r.text();
  try {
    return JSON.parse(text);
  } catch {
    return { status: r.status, body: text.slice(0, 100) };
  }
}

// 请求-响应配对的 WS 会话。E4 每发一条命令恰收一条回复；
// E1 的批量消息不取回复（服务端会回 echo，Node 无监听器即丢弃）。
function wsConnect(name) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${BASE}/ws?do=${name}&t=${TOKEN}`, PROXY ? { agent: new HttpsProxyAgent(PROXY) } : {});
    ws.on("open", () => {
      resolve({
        ws,
        send: (cmd) => ws.send(cmd),
        req: (cmd, timeoutMs = 10000) =>
          new Promise((res, rej) => {
            const t = setTimeout(() => rej(new Error("recv timeout for " + cmd)), timeoutMs);
            ws.once("message", (d) => {
              clearTimeout(t);
              res(JSON.parse(d.toString()));
            });
            ws.send(cmd);
          }),
        close: () => ws.close(),
      });
    });
    ws.on("error", (e) => reject(e));
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const b64 = (s) =>
  Buffer.isBuffer(s) ? s.toString("base64") : Buffer.from(s).toString("base64");

// example.com A 的 DNS 查询报文：向 9.9.9.9:53 的空闲 TCP socket 写入后应收到应答，
// 以此作为"socket 仍然活着"的正向证据。
function dnsQuery() {
  const head = Buffer.from([0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00]);
  const name = Buffer.concat([
    Buffer.from([7]), Buffer.from("example"),
    Buffer.from([3]), Buffer.from("com"),
    Buffer.from([0, 0, 1, 0, 1]),
  ]);
  return Buffer.concat([head, name]);
}

// ---- E1: WS 消息计数跨休眠持久（20:1 折算的对照数据） ----
async function e1() {
  const c = await wsConnect("e1");
  for (let i = 0; i < 100; i++) {
    c.send("ping:" + i);
    await sleep(20);
  }
  await sleep(1500);
  const mid = await http("/do/e1/count");
  report("E1-mid", { expect: "101 = 1 upgrade + 100 messages", mid });

  console.log("E1: idling 40s to let the DO hibernate ...");
  await sleep(40000);
  for (let i = 0; i < 50; i++) {
    c.send("pong:" + i);
    await sleep(20);
  }
  await sleep(1500);
  const fin = await http("/do/e1/count");
  c.close();
  report("E1-final", { expect: "152 = 101 + 50 + close; dashboard 'Requests' delta is the 20:1 ground truth", fin });
}

// ---- E2: 并发出站（DO 内 vs Worker 内） ----
async function e2() {
  const inDo = await http("/do/p/concurrent?n=12");
  const inWorker = await http("/w/concurrent?n=12");
  report("E2", {
    doSucceeded: inDo.succeeded,
    workerSucceeded: inWorker.succeeded,
    doResults: inDo.results?.map((r) => (r.ok ? "ok" : r.error)),
    workerResults: inWorker.results?.map((r) => (r.ok ? "ok" : r.error)),
  });
}

// ---- E3: IPv6 字面量 / NAT64 合成 / DoH ----
// NAT64 /96 合成 = 前缀(96bit) | IPv4(32bit)，8.8.8.8 → ::808:808。
// 前缀覆盖：level66 2001:67c:2960:6464::/96、well-known 64:ff9b::/96、
// Trex 2001:67c:2b::/96；裸 IPv6 对照组看 IPv6 出站本身通不通。
async function e3() {
  const tests = {
    v4_google_443: { host: "8.8.8.8", port: 443 },
    nat64_level66_443: { host: "2001:67c:2960:6464::808:808", port: 443 },
    nat64_level66_53: { host: "2001:67c:2960:6464::808:808", port: 53 },
    nat64_wkp_443: { host: "64:ff9b::808:808", port: 443 },
    nat64_wkp_53: { host: "64:ff9b::808:808", port: 53 },
    nat64_trex_53: { host: "2001:67c:2b::808:808", port: 53 },
    nat64_net_53: { host: "2a00:1098:2b::808:808", port: 53 },
    v6_google_53: { host: "2001:4860:4860::8888", port: 53 },
    v6_google_853: { host: "2001:4860:4860::8888", port: 853 },
    v6_cf_443: { host: "2606:4700:4700::1111", port: 443 },
  };
  const out = {};
  for (const [k, t] of Object.entries(tests)) {
    out[k] = await http(`/do/p/connect?host=${encodeURIComponent(t.host)}&port=${t.port}`);
  }
  const doh = await http("/w/doh?name=cloudflare.com");
  report("E3", { ...out, doh });
}

// ---- E4: 出站 socket 跨休眠存活 ----
async function e4() {
  const c = await wsConnect("e4");
  const opened = await c.req("open:E4:9.9.9.9:53");
  const meta1 = await c.req("meta");
  report("E4-open", { opened, meta1 });

  console.log("E4: idling 75s (alarm fires at +20s; runtime hibernates ~10s idle) ...");
  await sleep(75000);

  const meta2 = await c.req("meta");
  const check = await c.req("check:E4");
  let write = null;
  if (check.where !== "none") {
    write = await c.req("write:E4:" + b64(dnsQuery()), 15000);
  }
  const count = await http("/do/e4/count");
  c.close();
  report("E4-result", { meta2, check, write, persisted: count });
}

// ---- E5: 出站 socket 跨 DO 驱逐（无 WS 客户端）后的存活 ----
// 流程：开 socket -> 验证 echo 目标可用 -> 客户端断开 WS -> 等待 DO 被驱逐 ->
// 新 WS 连回同一 DO -> 看 isolateBorn / instanceSockets / globalSockets，
// 再 check/write 全局表里的 socket。
async function e5() {
  const c1 = await wsConnect("e5");
  const opened = await c1.req("open:E5:tcpbin.com:4242", 15000);
  const write1 = await c1.req("write:E5:" + b64("ping-e5\n"), 15000);
  c1.close();
  console.log("E5: ws closed; idling 45s to let the DO be evicted ...");
  await sleep(45000);
  const c2 = await wsConnect("e5");
  const meta = await c2.req("meta");
  const check = await c2.req("check:E5");
  let write2 = null;
  if (check.where !== "none") {
    write2 = await c2.req("write:E5:" + b64("ping-e5-after\n"), 15000);
  }
  c2.close();
  report("E5-result", { opened, write1, meta, check, write2 });
}

(async () => {
  await e1();
  await e2();
  await e3();
  await e4();
  await e5();
  writeFileSync("drive-results.json", JSON.stringify(out, null, 2));
  console.log("\nwrote drive-results.json");
})().catch((e) => {
  console.error("driver failed:", e);
  console.log(JSON.stringify(out, null, 2));
  writeFileSync("drive-results.json", JSON.stringify(out, null, 2));
  process.exit(1);
});
