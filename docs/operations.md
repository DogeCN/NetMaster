# 运维

## 服务端

### 部署要配什么

**三个 GitHub Secret，一次 push，然后自己绑域名。** 没有别的。

| Secret | 用途 |
|---|---|
| `CLOUDFLARE_API_TOKEN` | CI 调 Cloudflare API（需 Workers Scripts / D1 编辑权限） |
| `CLOUDFLARE_ACCOUNT_ID` | 账号 ID |
| `PASSWORD` | 客户端连接口令，CI 透传成 Worker Secret |

```bash
gh secret set CLOUDFLARE_API_TOKEN --repo <你>/<仓库>
gh secret set CLOUDFLARE_ACCOUNT_ID --repo <你>/<仓库>
gh secret set PASSWORD --repo <你>/<仓库>
git push
```

CI 自动：跑测试 → 建同名 D1 → 应用 schema（幂等）→ 部署 Worker → 透传 PASSWORD。

**CI 不做部署后验证**，这是刻意的：GitHub runner 在美国，用户在大陆，runner 能通
不代表用户能通，反过来也一样。验证部署的方式就是拿客户端连一次。域名绑定是
用户自己的事（控制台加 custom domain，或自己 wrangler 一行），项目不持有域名。

### 鉴权：PASSWORD

- 客户端与 Worker 各自计算 `auth = md5(utf8(PASSWORD))`，网络只出现派生值
  （16 字节，TLS 之内）。
- 改口令 = 改 GitHub Secret 重新部署 + 改客户端参数，两端同步即可。
- 没设 PASSWORD 时部署仍会成功，但 Worker 拒绝一切连接（upgrade 时返回 503）。
- 派生值没有任何"默认可推算"的版本 —— 没有口令就没有派生值，Worker 明确报错
  而不是退化到共享凭据。

### D1

只绑定一张表：

| 表 | 用途 |
|---|---|
| `relay_binding` | 域名 → 上次可用的中继（中继亲和的持久层） |

`schema.sql` 全部是 `CREATE IF NOT EXISTS`，每次部署都会跑一遍 —— 之后新增表
不需要迁移逻辑，重跑天然安全。

### 构建与部署

```bash
cd server
npm install                # 只有测试需要（ws）
node build.mjs             # src/ -> _worker.js
```

`_worker.js` 是**产物**，不要手改。

**正式部署路径是 CI**：`git push` 触发 `.github/workflows/deploy.yml`。

### 环境变量

只剩一个：

| 变量 | 默认 | 说明 |
|---|---|---|
| `DEBUG` | 关 | `1`/`true` 打开服务端日志（`wrangler tail` 里看） |

其余一切都不是变量：鉴权是 PASSWORD secret；中继列表是动态获取 + 内置兜底；
入口候选在客户端本地组装。

### 客户端

```bash
cd client && go build -o netmaster.exe ./cmd/netmaster
netmaster serve --server <域名> --password <PASSWORD>
```

配置来源优先级：**命令行 flag > config.json > 默认值**。config.json 用标准库
解析（零依赖），可配置项与 serve 的 flag 一一对应，写在当前目录或
`%AppData%/netmaster/config.json`：

```json
{ "server": "<域名>", "password": "<口令>", "manual": false, "rules": "" }
```

写好之后日常就是裸的 `netmaster serve`。环境变量 `NETMASTER_SERVER` /
`NETMASTER_PASSWORD`（`PASSWORD` 也认）是最后一级兜底，优先级低于 config.json。
缺 server/password 时以退出码 2 报错，而不是拿空凭据去连。

**双击 exe 等价于 `serve`**：无参数启动即 serve；首次双击没有 config.json 时
在 exe 旁生成模板，填好 server/password 再点一次；任何启动错误都会等一次
回车再关窗口，不让人盯着一闪而过的黑框猜原因。注意直接关闭窗口 = 强杀进程，
系统代理由看门狗还原（这正是它存在的意义）。

serve 的 flag 收敛为 4 个：

| 参数 | config.json 键 | 说明 |
|---|---|---|
| `--server` | `server` | 服务端域名（裸域名即可，scheme 会被剥掉） |
| `--password` | `password` | 部署口令 |
| `--manual` | `manual` | 不接管系统代理，只打印监听地址 |
| `--rules` | `rules` | 自定义分流规则文件 |

其余行为都是固定的好默认值：ECH 开、geoip 开、后台探测、mux 复用。

### 端口自动选择

HTTP 先试 8080、SOCKS5 先试 1080，被占则顺延找空闲端口，结果打印到终端。
系统代理自动指向选定的 HTTP 端口，用户通常不需要知道端口号；`--manual` 下
只打印地址，不接管。

### 首次连通验证

ready 之后 serve 在后台做一次真实的传输层建连（TLS+WS+auth），补一行日志：

```
tunnel established via node [3] 104.17.x.x
tunnel failed: <原因>
```

部署是否健康，这一行就是最直接的回答（Worker 没有健康检查端点，CI 也不做
部署后验证 —— 这一行就是验证）。

### 子命令

| 命令 | 用途 |
|---|---|
| `serve` | 日常唯一命令 |
| `nodes` | 持续发请求，观察自适应选路的实时效果；`--ipcheck` 统计出口 IP 分布 |
| `restore` | 崩溃/强杀后手动还原系统代理 |

`watchdog` 是内部命令，由 serve 自动派生，不面向用户。

### 缓存位置

`%LOCALAPPDATA%/netmaster/`：`entry-*.json`（社区优选）、`probe-*.json`（节点延迟）、
`state-*.json`（域名亲和 + 直连冷却）、`geoip-*.json`（CN 网段）。删掉即可强制
重新拉取/重新学习（serve 没有 --refresh flag，删缓存就是强制刷新的方式）。

`state-*.json` 值得单独说明：里面是客户端**学到的**路由知识（哪个域名走哪个出口、
哪些域名直连被阻断过）。落盘意味着重启不丢 —— 学到的最有价值的知识不该活不过
一次重启。换服务端域名时旧状态天然失效（按域名做缓存键）。

## 系统代理的接管与还原

顺序是刻意的：

```
1. 代理开始监听
2. 清理上次残留的状态（异常退出后系统代理可能还指着不存在的端口）
3. 设置系统代理（先落盘旧值，再逐项写注册表）
4. 拉起看门狗（仅在第 3 步成功时）
5. ready
```

- **先监听再设代理**：避免出现"系统代理已指向、端口还没起来"的空窗。
- **`Enable` 先落盘再改注册表**：反过来的话，进程在两者之间挂掉会造成"注册表已改但状态
  文件不存在"，看门狗和下次启动的清理都检测不到，系统代理会一直悬着。
- **看门狗只在接管了系统代理时拉起并落盘状态**：`--manual` 下不做；设置失败同样不留
  空转进程。它阻塞等主进程退出，若状态文件仍在（说明主进程没走正常清理）就还原。
- **`Enable` 中途失败会立即回滚**：它可能已经改了一部分注册表（比如 `ProxyEnable` 已置 1），
  而调用方把错误当作"什么都没发生"，退出时就不还原了。

## 更新

服务端和客户端可独立升级，协议不变。两端版本不匹配的表现是客户端连不上
（协议字段变化）—— 两端都在这个仓库里，同时升级即可。
