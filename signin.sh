#!/bin/bash
# ═══ 更新日志 ═══
# 2026-09-25：完整运行包中的签到辅助脚本沿用持久化账号目录。
# 批量签到脚本：遍历 auths/ 下所有 workbuddy-*.json 账号
# 用法: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

BIN=./signin_bin
if [ ! -x "$BIN" ]; then
    echo "build signin_bin ..."
    go build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-${WB2A_AUTH_DIR:-${WB2A_AUTHS:-${WB2API_GATEWAY_DIR:-.}/auths}}}"
