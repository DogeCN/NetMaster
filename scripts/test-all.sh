#!/usr/bin/env bash
# 全量测试：服务端 + 客户端
set -euo pipefail
# 绝对路径：脚本会 cd 到别处，相对路径会失效
here="$(cd "$(dirname "$0")" && pwd)"

echo "== 服务端 =="
cd "$here/../server"
[ -d node_modules ] || npm install --no-audit --no-fund
node build.mjs
# crypto/protocol 是纯逻辑；其余套件经 socket.js 摸平台模块，要挂 Node shim
for t in crypto protocol; do
  printf '  %-12s ' "$t"
  node "test/$t.mjs" | tail -1
done
for t in integration proxyip order; do
  printf '  %-12s ' "$t"
  NODE_OPTIONS="--import ./test/shims/register.mjs" node "test/$t.mjs" | tail -1
done

echo "== 客户端 =="
cd "$here/../client"
go vet ./...
test -z "$(gofmt -l .)" || { echo "gofmt 未完成:"; gofmt -l .; exit 1; }
go test ./... 2>&1 | grep -v "no test files"
