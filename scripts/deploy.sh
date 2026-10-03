#!/usr/bin/env bash
# 一键部署服务端：给不想用 GitHub Actions 的人（clone 之后自己 wrangler deploy）。
#
# 与 CI 的差别只有一处：CI 用 CF REST API 解析 namespace id 后就地 sed 改写
# wrangler.toml（runner 是一次性的，改了没人看见）；这里是用户自己的仓库，
# 改入库文件会把工作区弄脏（git status 里冒出一个被改的 wrangler.toml，
# 下次 pull 还可能冲突）。所以本脚本生成一份临时配置再部署，原文件不动。
#
# 没有部署后验证：CI 的网络在美国、用户的网络在大陆，一边能通不代表另一边能通。
# 部署是否健康由"客户端能不能连上"直接回答 —— Worker 没有 HTTP 端点来回答
# 这个问题（打开域名只会得到 404，那是预期行为）。
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
server_dir="$here/../server"
toml_src="$server_dir/wrangler.toml"
toml_tmp="$server_dir/wrangler.deploy.toml"
placeholder="00000000000000000000000000000000"

dry_run=0

# 用法
usage() {
  cat <<'EOF'
usage: scripts/deploy.sh [--dry-run] [--help]

Deploy the NetMaster Worker to Cloudflare with wrangler.

Options:
  --dry-run   print the commands it would run, deploy nothing
  --help      show this message

Prerequisites:
  - wrangler on PATH (or reachable via npx)
  - logged in:  npx wrangler login
  - PASSWORD set in the environment, e.g.
      read -r -s PASSWORD && export PASSWORD
    (or let the script prompt you for it)

What it does:
  1. npm ci
  2. node build.mjs          -> server/_worker.js
  3. syntax check on _worker.js
  4. resolve/create the KV namespace 'netmaster'
  5. copy wrangler.toml -> wrangler.deploy.toml with the real namespace id
  6. wrangler deploy -c wrangler.deploy.toml
  7. wrangler secret put PASSWORD

The custom domain is NOT handled here. Bind it yourself in the Cloudflare
dashboard (or with wrangler) — the client can only reach the Worker through it.
EOF
}

# run 打印要执行的命令再执行。dry-run 下只打印。
run() {
  printf '$ %s\n' "$*"
  if [ "$dry_run" -eq 0 ]; then
    "$@"
  fi
}

# wrangler 调用统一走这个函数：本机可能没装全局 wrangler，server/ 的
# devDependencies 里有，npx 能找到。
wr() {
  if command -v wrangler >/dev/null 2>&1; then
    run wrangler "$@"
  else
    run npx --no-install wrangler "$@"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=1 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'unknown option: %s\n\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

# --- 前置检查 ---

# wrangler 可用性。npx --no-install 找不到时会非零退出，比直接跑 wrangler
# 拿到 "command not found" 更可控（后者在不同 shell 下报的错不一样）。
if command -v wrangler >/dev/null 2>&1; then
  :
elif [ -d "$server_dir/node_modules" ] && [ -x "$server_dir/node_modules/.bin/wrangler" ]; then
  :
elif npx --no-install wrangler --version >/dev/null 2>&1; then
  :
else
  echo "error: wrangler not found." >&2
  echo "  install it with:  cd server && npm ci" >&2
  echo "  then re-run this script." >&2
  exit 1
fi

# PASSWORD 是唯一的必需 secret。没有它部署会成功，但 Worker 会拒绝一切客户端
# 连接（首帧 HMAC 对不上），用户只会看到"连不上"而看不到原因 —— 提前拦住。
if [ -z "${PASSWORD:-}" ]; then
  # 交互式终端下直接问，避免用户因为一次空口令白跑一趟完整部署。
  if [ -t 0 ]; then
    printf 'PASSWORD (the client must use the same one): '
    read -r -s PASSWORD
    printf '\n'
    export PASSWORD
  fi
  if [ -z "${PASSWORD:-}" ]; then
    echo "error: PASSWORD is not set." >&2
    echo "  export PASSWORD='<your password>' and re-run," >&2
    echo "  or set it afterwards with:  npx wrangler secret put PASSWORD" >&2
    exit 1
  fi
fi

# --- 1~3. 依赖与构建 ---
cd "$server_dir"

run npm ci
run node build.mjs

# _worker.js 是拼接产物：build.mjs 只做字符串拼接，语法错误会一路带到部署之后
# 才以 Worker 启动失败的形式暴露。这里校验一次，让它在部署前就炸出来。
#
# 用 `node --check` 而不是 `import('./_worker.js')`：产物顶层有 cloudflare:* 平台
# import（socket.js 的 connect、session.js 的 DurableObject），Node 的 ESM 加载器
# 根本不认识这个 scheme，会抛 ERR_UNSUPPORTED_ESM_URL_SCHEME —— 那是"Node 不支持",
# 不是"产物坏了"。原来的 import 检查因此必然失败，等于这条部署路径从来跑不通。
# 与 ci.yml 的 build 门保持一致：那里也是 node --check。
if [ "$dry_run" -eq 0 ]; then
  node --check _worker.js && echo "bundle syntax OK"
else
  printf '$ %s\n' "node --check _worker.js   # bundle syntax check"
fi

# --- 4. KV namespace ---
#
# Cron 每小时把出口健康排名写进 KV（key: proxyip:top）。没有 KV 绑定时竞速仍然
# 能跑（只用内置兜底列表），所以这一步失败不该中断部署：打印提示继续即可。
#
# `wrangler kv namespace list` 输出的是 JSON 数组（wrangler 4.x 源码里就是
# JSON.stringify），所以按 title 精确取 id，而不是从人类可读文本里 grep 第一个
# 32 位十六进制串——账号里有多个 namespace 时后者会取错。
kv_id=""
if [ "$dry_run" -eq 0 ]; then
  kv_list="$(wrangler kv namespace list 2>/dev/null || true)"
  kv_id="$(printf '%s' "$kv_list" | node -e '
    let d = "";
    process.stdin.on("data", (c) => (d += c)).on("end", () => {
      try {
        const list = JSON.parse(d);
        const hit = (Array.isArray(list) ? list : []).find((x) => x && x.title === "netmaster");
        process.stdout.write(hit ? String(hit.id) : "");
      } catch {
        process.stdout.write("");
      }
    });' 2>/dev/null || true)"
fi

if [ -z "$kv_id" ]; then
  echo "== KV namespace 'netmaster' not found; creating it =="
  if [ "$dry_run" -eq 0 ]; then
    # 对 TOML 配置，wrangler 只会打印要加的配置片段，不会自己改写 wrangler.toml
    # （自动写入只发生在 JSON 配置格式下），所以这一步不会污染入库文件。
    create_out="$(wrangler kv namespace create netmaster 2>&1 || true)"
    printf '%s\n' "$create_out"
    kv_id="$(printf '%s' "$create_out" | grep -oE '[0-9a-f]{32}' | head -1 || true)"
  else
    printf '$ %s\n' "wrangler kv namespace create netmaster"
    kv_id="$placeholder"
  fi
fi

# --- 5. 生成临时配置 ---
#
# 为什么必须用临时文件：wrangler.toml 是入库文件，就地改写会污染工作区
# （git status 多一个改动、下次 pull 可能冲突、用户以为自己改过配置）。
# 从它复制一份再替换占位 id，部署完留在 server/ 下，且已在 .gitignore 里。
if [ -z "$kv_id" ]; then
  echo "warn: could not resolve a KV namespace id — deploying without rewriting it." >&2
  echo "      racing will fall back to the built-in relay list only." >&2
  cp "$toml_src" "$toml_tmp"
else
  if [ "$dry_run" -eq 0 ]; then
    sed "s/^id = \"$placeholder\"/id = \"$kv_id\"/" "$toml_src" > "$toml_tmp"
    echo "== wrangler.deploy.toml written (KV id $kv_id); wrangler.toml untouched =="
  else
    printf '$ %s\n' "sed \"s/^id = \\\"$placeholder\\\"/id = \\\"<KV_ID>\\\"/\" wrangler.toml > wrangler.deploy.toml"
  fi
fi

# --- 5b. 可选：打开服务端 profile ---
#
# PROFILE=1 scripts/deploy.sh 会在**临时配置**上追加 [vars] PROFILE = "1"，
# 于是 SessionDO 开始把分段耗时写进 KV（键 profile:<目标>:<分钟>，TTL 1 小时）。
# 客户端侧对应的是环境变量 NETMASTER_PROFILE=1，见 tools/profile.mjs。
#
# 为什么只改临时配置、不改 wrangler.toml：
#   1. profile 要花 KV 写配额。它是排障开关，不该留在入库配置里变成常开 ——
#      否则每次部署都在为一个没人看的功能写 KV。
#   2. wrangler.toml 里 keep_vars = false，语义是"删掉 Worker 上有、本文件里没有
#      的变量"。如果只在控制台的 Variables 里加 PROFILE，下一次 wrangler deploy
#      会把它悄悄删掉，而本地验证时明明是好的 —— 这种"部署一次就没了"最难查。
#      写进实际部署的那份配置就没这个问题。
if [ "$dry_run" -eq 0 ]; then
  if [ "${PROFILE:-}" = "1" ]; then
    printf '\n[vars]\nPROFILE = "1"\n' >> "$toml_tmp"
    echo "== profile ON (KV keys profile:<target>:<minute>, ttl 1h) =="
  fi
else
  if [ "${PROFILE:-}" = "1" ]; then
    printf '$ %s\n' "printf '\\n[vars]\\nPROFILE = \"1\"\\n' >> wrangler.deploy.toml"
  fi
fi

# --- 6. 部署 ---
if [ "$dry_run" -eq 0 ]; then
  wrangler deploy -c "$toml_tmp"
else
  printf '$ %s\n' "wrangler deploy -c wrangler.deploy.toml"
fi

# --- 7. 透传 PASSWORD ---
# printf 而不是 echo：echo 会追加换行符，那个换行会变成口令的一部分，
# 客户端和服务端于是永远对不上。
if [ "$dry_run" -eq 0 ]; then
  printf '%s' "$PASSWORD" | wrangler secret put PASSWORD -c "$toml_tmp"
else
  printf '$ %s\n' "printf '%s' \"\$PASSWORD\" | wrangler secret put PASSWORD -c wrangler.deploy.toml"
fi

if [ "$dry_run" -eq 1 ]; then
  echo
  echo "(--dry-run: nothing was deployed)"
  exit 0
fi

cat <<'EOF'

Deployed. Remaining step (not done by this script):

  bind a custom domain to the Worker in the Cloudflare dashboard.
  The client reaches the Worker only through that domain — *.workers.dev
  alone is not enough for most users.

Then run the client and watch for this line:

  tunnel established via node <addr>

No /health endpoint exists; that line is the only direct answer to
"is the deployment healthy?".
EOF
