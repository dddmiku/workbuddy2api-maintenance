#!/usr/bin/env bash
# ═══ 更新日志 ═══
# 2026-10-12：初版——Trae 邀请奖励补发（薄包装，转发给 fission_bin）。
# fission.sh — Trae 邀请奖励补发辅助工具
#
# 面板上邀请记录停在「邀请中」，是因为发奖动作在 TRAE 桌面端本地：客户端登录时
# 会自己调 POST /trae/api/v2/ug/code_fission/grant（body {"activity_id":
# "code_fission_202609"}）。被邀请人没登过桌面端，记录就永远挂着。本工具把
# 那一次调用搬到服务端补发。
#
# 用法:
#   ./fission.sh                        # 扫描全部 trae 账号补发
#   ./fission.sh -uid 2081842786229369  # 只处理指定账号（可重复）
#   ./fission.sh -gen 7                 # 用第 7 代设备号
#   ./fission.sh -dry-run               # 只探测（查邀请码 + 当前积分），不发放
#   ./fission.sh -pretty                # 人类可读输出（默认 JSON）
#   ./fission.sh auths_dir              # 位置参数=账号目录
#
# 关于 -gen：活动规则「同一台电脑只能被邀请一次」落到实现就是 x-device-id。
# 上游返回 9091 时会自动换 gen 重试一次；手动指定用于已知被判重的情况。
#
# 依赖: go（首次构建 fission_bin；容器内镜像已带 /app/fission_bin）
set -euo pipefail
cd "$(dirname "$0")"

AUTHS_DIR="${WB2A_AUTH_DIR:-${WB2A_AUTHS:-${WB2API_GATEWAY_DIR:-.}/auths}}"
ARGS=()
for arg in "$@"; do
    case "$arg" in
        -*) ARGS+=("$arg") ;;
        *)  AUTHS_DIR="$arg" ;;
    esac
done

# 二进制解析优先序：1) 容器内预置 /app/fission_bin（镜像带产物，无需 go）；
# 2) $CACHE_DIR 缓存；3) 源码更新则重编（需 go 工具链，本地/CI 用）。
FISSION_BIN="$(cd "$(dirname "$0")" && pwd)/fission_bin"
if [[ ! -x "$FISSION_BIN" ]]; then
    CACHE_DIR="${TMPDIR:-/tmp}/workbuddy2api-bin"
    mkdir -p "$CACHE_DIR"
    FISSION_BIN="$CACHE_DIR/fission_bin"
    if [[ ! -x "$FISSION_BIN" ]] || find . -name '*.go' -newer "$FISSION_BIN" -print -quit | grep -q .; then
        if ! command -v go >/dev/null 2>&1; then
            echo "需要 go 构建 fission_bin（或镜像内置 /app/fission_bin）" >&2
            exit 1
        fi
        echo "构建 fission.." >&2
        go build -o "$FISSION_BIN" ./cmd/fission
    fi
fi

exec "$FISSION_BIN" -auths "$AUTHS_DIR" "${ARGS[@]+"${ARGS[@]}"}"
