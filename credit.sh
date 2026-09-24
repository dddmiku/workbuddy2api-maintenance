#!/usr/bin/env bash
# ═══ 更新日志 ═══
# 2026-09-25：完整运行包中的CLI辅助脚本沿用持久化账号目录。
# credit.sh — WorkBuddy 积分日报（默认美化输出）
#
# 用法:
#   ./credit.sh            # 人类可读日报
#   ./credit.sh -json      # 原始 JSON
#
# 二进制升级: go build -o credit ./cmd/credit
set -euo pipefail
cd "$(dirname "$0")"
export WB2A_AUTH_DIR="${WB2A_AUTH_DIR:-${WB2A_AUTHS:-${WB2API_GATEWAY_DIR:-.}/auths}}"
if [[ "${1:-}" == "-json" ]]; then
    exec ./credit
fi
exec ./credit -pretty
