-- NetMaster D1 schema —— 只有一张表。
--
-- 每次 deploy 都会执行（deploy.yml），全部语句幂等，可安全重跑。

-- 域名 → 中继 亲和。中继的出口 IP 决定 Cloudflare 系站点给 200 还是 403，
-- 所以一个中继对某域名走通后就记住它。按域名而不是全局记：被一个站拉黑的
-- 中继对另一个站可能是好的，全局"最近可用的中继"会被最后访问的站点反复重置。
CREATE TABLE IF NOT EXISTS relay_binding (
    host        TEXT PRIMARY KEY,
    relay       TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL
);

-- Lets an expired binding be found without scanning the whole table.
CREATE INDEX IF NOT EXISTS idx_relay_updated
    ON relay_binding(updated_at);
