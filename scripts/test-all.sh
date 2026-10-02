#!/usr/bin/env bash
# 全量测试：服务端 + 客户端
set -euo pipefail
# 绝对路径：脚本会 cd 到别处，相对路径会失效
here="$(cd "$(dirname "$0")" && pwd)"

echo "== 服务端 =="
cd "$here/../server"
[ -d node_modules ] || npm install --no-audit --no-fund
node build.mjs
for t in crypto protocol integration proxyip race router cron; do
  printf '  %-12s ' "$t"
  node "test/$t.mjs" | tail -1
done

echo "== 客户端 =="
cd "$here/../client"
go vet ./...
test -z "$(gofmt -l .)" || { echo "gofmt 未完成:"; gofmt -l .; exit 1; }
go test ./... 2>&1 | grep -v "no test files"
