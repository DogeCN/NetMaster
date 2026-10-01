#!/usr/bin/env bash
# 构建服务端产物 server/_worker.js
set -euo pipefail
cd "$(dirname "$0")/../server"
[ -d node_modules ] || npm install --no-audit --no-fund
node build.mjs
