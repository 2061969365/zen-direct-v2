#!/bin/sh
# zen-direct v2 常驻启动器（Linux）。与 run-gateway.ps1 的环境变量保持一致。
#
# 用法：
#   ./start-linux.sh              # 前台启动（systemd/docker 里用这个）
#   ./start-linux.sh --daemon     # 后台启动，写 pid 到 state/zen-direct.pid
#
# 前置：同目录下有可执行的 zen-direct（或 zen-direct-linux-amd64）。
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$HERE"

# ===================== CONFIG =====================
PORT="${PORT:-8081}"
CONCURRENCY="${DIRECT_CONCURRENCY:-16}"
LOG_MAX_MB="${LOG_MAX_MB:-200}"
ZEN_BASE="${OPENCODE_ZEN_BASE:-https://opencode.ai/inference/v1}"
FRESH_PER_TURN="${DIRECT_FRESH_PER_TURN:-1}"
# ==================================================

STATE_DIR="$HERE/state"
LOG_DIR="$HERE/logs"
mkdir -p "$STATE_DIR" "$LOG_DIR"

# 找可执行文件：优先 zen-direct（Railway/entrypoint 约定），回退交叉编译产物名
BIN=""
for candidate in "$HERE/zen-direct" "$HERE/zen-direct-linux-amd64" ; do
  if [ -f "$candidate" ]; then BIN="$candidate"; break; fi
done
if [ -z "$BIN" ]; then
  echo "missing zen-direct binary next to $0" >&2
  exit 1
fi
chmod +x "$BIN" 2>/dev/null || true

# 只收掉自己那一个进程（有 pid 文件时）
PID_FILE="$STATE_DIR/zen-direct.pid"
if [ -f "$PID_FILE" ]; then
  OLD=$(cat "$PID_FILE" 2>/dev/null || true)
  if [ -n "${OLD:-}" ] && kill -0 "$OLD" 2>/dev/null; then
    kill "$OLD" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$PID_FILE"
fi

export PORT
export DIRECT_DEBUG="${DIRECT_DEBUG:-1}"
export DIRECT_DIAG="${DIRECT_DIAG:-1}"
export DIRECT_CONCURRENCY="$CONCURRENCY"
export LOG_MAX_MB
export OPENCODE_ZEN_BASE="$ZEN_BASE"
export DIRECT_FRESH_PER_TURN="$FRESH_PER_TURN"
export SHIM_SESSION_FILE="${SHIM_SESSION_FILE:-$STATE_DIR/shim_sessions.json}"
export GENUINE_SESSION_FILE="${GENUINE_SESSION_FILE:-$STATE_DIR/genuine_sessions.txt}"
# 原生传输：uTLS 复刻 Bun ClientHello + fetch 头序；不拉起 bun sidecar
export ZEN_TLS="${ZEN_TLS:-bun}"
export DIRECT_SIDECAR="${DIRECT_SIDECAR:-off}"

case "${1:-}" in
  --daemon)
    LOG="${LOG_FILE:-$LOG_DIR/zen-direct-$(date +%Y%m%d).err.log}"
    export LOG_FILE="$LOG"
    nohup "$BIN" >>"$LOG" 2>&1 &
    echo $! >"$PID_FILE"
    sleep 2
    echo "started pid $(cat "$PID_FILE"), log $LOG"
    if command -v curl >/dev/null 2>&1; then
      curl -s -m 5 "http://127.0.0.1:$PORT/health" || echo "health check failed, see $LOG" >&2
      echo
    fi
    ;;
  *)
    exec "$BIN"
    ;;
esac
