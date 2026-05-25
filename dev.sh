#!/usr/bin/env bash
# Codex Viewer 启动脚本 —— 编译并运行会话实时查看器
set -euo pipefail
cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  echo "✗ 未检测到 Go，请先安装：https://go.dev/dl/"
  exit 1
fi

BIN="./codex-ui"
echo "  ◆ 正在编译 Codex Viewer ..."
go build -o "$BIN" .
echo "  ◆ 编译完成，启动服务 ..."
echo ""

# 可选参数：传入自定义会话目录，例如  ./dev.sh /path/to/sessions
exec "$BIN" "$@"
