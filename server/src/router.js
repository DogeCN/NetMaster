// Router DO（PRD §7.5）：目标 → 出口路径的全局映射，SQLite 存，异步批量 flush。
//
// 只存"这个目标上次走通了哪条出口"，不存数据。会话级内存缓存管本连接内的复用，
// 跨会话的复用完全靠这里；条目 TTL 1 小时，与 Cron 周期对齐，避免抖动目标长期
// 滞留死映射。
//
// 写入路径刻意是"先返回、后落盘"：learn 不排在 socket 建立的临界路径上，
// 攒 5 秒或 50 条 flush 一次，DO 被驱逐时丢掉的未 flush 数据可以从竞速重建。

export const ROUTE_TTL_MS = 3600 * 1000;
export const FLUSH_BATCH_MAX = 50;
export const FLUSH_DELAY_MS = 5000;

// 分片接口预留（PRD §7.5）：现在全局单实例，行数或速率越线时按目标哈希切 16 片，
// 调用方只需把 idFromName(routerName(shardId)) 换掉。
export const ROUTER_SHARDS = 16;
export function routerName(shardId = 0) {
  return `router:${shardId}`;
}
export function routerShardId(hash) {
  return parseInt(String(hash).slice(0, 4), 16) % ROUTER_SHARDS;
}

const CREATE_ROUTES_SQL =
  "CREATE TABLE IF NOT EXISTS routes (target_hash TEXT PRIMARY KEY, egress_type TEXT, egress_id TEXT, updated_at INTEGER)";
const UPSERT_ROUTE_SQL =
  "INSERT INTO routes (target_hash, egress_type, egress_id, updated_at) VALUES (?, ?, ?, ?) " +
  "ON CONFLICT(target_hash) DO UPDATE SET egress_type = excluded.egress_type, egress_id = excluded.egress_id, updated_at = excluded.updated_at";
const SELECT_ROUTE_SQL = "SELECT egress_type, egress_id, updated_at FROM routes WHERE target_hash = ?";
const DELETE_ROUTE_SQL = "DELETE FROM routes WHERE target_hash = ?";

// target_hash = 目标域名（小写）SHA-256 前 16 字节十六进制。
// 只存哈希不存域名：路由表是缓存，不是访问日志，没必要留可还原的目标名。
export async function targetHash(host) {
  const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(String(host).toLowerCase()));
  return Array.from(new Uint8Array(d).slice(0, 16))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

// routeFresh 判断条目是否仍在 TTL 内（updated_at + TTL < now 视为未命中）。
export function routeFresh(row, nowMs = Date.now()) {
  if (!row) return false;
  const t = Number(row.updated_at);
  if (!Number.isFinite(t)) return false;
  return nowMs - t < ROUTE_TTL_MS;
}

// parseLearn 校验 learn/forget 的请求体；不合法返回 null。
export function parseLearn(body) {
  const hash = String(body?.hash || "");
  if (!/^[0-9a-f]{32}$/.test(hash)) return null;
  const type = String(body?.type || "");
  const id = String(body?.id || "");
  if (!type || !id) return null;
  return { hash, type, id };
}

// RouteQueue 待写队列。同一个 hash 只留最新一条：连着 learn 三次和 learn 一次
// 对最终状态没有区别，没必要写三行。
export class RouteQueue {
  constructor(max = FLUSH_BATCH_MAX) {
    this.max = max;
    this.map = new Map();
  }
  get size() {
    return this.map.size;
  }
  empty() {
    return this.map.size === 0;
  }
  push(hash, type, id, updatedAt = Date.now()) {
    this.map.set(hash, { hash, type, id, updatedAt });
    return this.map.size;
  }
  // full 表示应当立即 flush。
  full() {
    return this.map.size >= this.max;
  }
  take(n = this.max) {
    const out = [];
    for (const row of this.map.values()) {
      if (out.length >= n) break;
      out.push(row);
    }
    for (const row of out) this.map.delete(row.hash);
    return out;
  }
}

// 注意：这里不 extends DurableObject——写成裸类，router.js 才能被 Node 单测直接
// import（Workers 侧两种写法的注册方式一致：wrangler 的 class_name）。
export class RouterDO {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.queue = new RouteQueue();
    this.alarmAt = null; // 本地镜像，避免每次 learn 都去读 alarm 状态
    this._sql = null;
  }

  log(msg) {
    if (String(this.env?.DEBUG || "") === "1") console.log(`[router] ${msg}`);
  }

  sql() {
    if (!this._sql) {
      this._sql = this.state.storage.sql;
      this._sql.exec(CREATE_ROUTES_SQL);
    }
    return this._sql;
  }

  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/lookup") return this.lookup(url.searchParams.get("hash"));
    if (request.method === "POST" && url.pathname === "/learn") return this.learn(await readJson(request));
    if (request.method === "POST" && url.pathname === "/forget") return this.forget(await readJson(request));
    return new Response(null, { status: 404 });
  }

  // lookup 命中且未过 TTL 才返回条目；其余一律 404（调用方回退到竞速）。
  async lookup(hash) {
    const h = String(hash || "");
    if (!/^[0-9a-f]{32}$/.test(h)) return new Response(null, { status: 404 });
    const row = this.sql()
      .exec(SELECT_ROUTE_SQL, h)
      .toArray()[0];
    if (!row || !routeFresh({ updated_at: row.updated_at })) return new Response(null, { status: 404 });
    return Response.json({ hash: h, type: row.egress_type, id: row.egress_id, updated_at: Number(row.updated_at) });
  }

  // learn 只入队并立刻返回。队列满 50 条立即 flush，否则挂一个 +5s 的 alarm。
  async learn(body) {
    const r = parseLearn(body);
    if (!r) return new Response(null, { status: 400 });
    this.queue.push(r.hash, r.type, r.id);
    if (this.queue.full()) await this.flush();
    else await this.ensureAlarm();
    return new Response(null, { status: 204 });
  }

  // forget 是失效路径，必须立刻生效，不能跟着队列一起等 5 秒。
  async forget(body) {
    const h = String(body?.hash || "");
    if (!/^[0-9a-f]{32}$/.test(h)) return new Response(null, { status: 400 });
    this.sql().exec(DELETE_ROUTE_SQL, h);
    this.queue.map.delete(h);
    await this.dropIdleAlarm();
    return new Response(null, { status: 204 });
  }

  async ensureAlarm() {
    if (this.alarmAt !== null) return;
    const at = Date.now() + FLUSH_DELAY_MS;
    await this.state.storage.setAlarm(at);
    this.alarmAt = at;
  }

  // flush 后队列已空就撤掉待定 alarm：空转唤醒要花 DO 时长与行写配额。
  async dropIdleAlarm() {
    if (this.alarmAt === null || !this.queue.empty()) return;
    await this.state.storage.deleteAlarm();
    this.alarmAt = null;
  }

  async flush() {
    const batch = this.queue.take(FLUSH_BATCH_MAX);
    if (batch.length === 0) {
      await this.dropIdleAlarm();
      return;
    }
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        this.writeBatch(batch);
        break;
      } catch (e) {
        // 重试 1 次，仍失败就丢弃这批：缓存可由竞速重建，不值得为它反复写。
        if (attempt === 1) this.log(`flush dropped ${batch.length} rows: ${e.message || e}`);
      }
    }
    await this.dropIdleAlarm();
  }

  writeBatch(batch) {
    const sql = this.sql();
    for (const row of batch) {
      sql.exec(UPSERT_ROUTE_SQL, row.hash, row.type, row.id, row.updatedAt);
    }
  }

  async alarm() {
    this.alarmAt = null;
    await this.flush();
  }
}

async function readJson(request) {
  try {
    return await request.json();
  } catch {
    return null;
  }
}