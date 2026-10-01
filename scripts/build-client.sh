#!/usr/bin/env bash
# 构建客户端
set -euo pipefail
cd "$(dirname "$0")/../client"
go build -o netmaster.exe ./cmd/netmaster
echo "built: netmaster.exe"
