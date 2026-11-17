#!/bin/sh
# build-linux.sh — 交叉/本机编译 Linux 版本（amd64 + arm64，静态、无 cgo）。
#
# 用法：
#   ./build-linux.sh            # 两个架构都出，产物落 dist/
#   ./build-linux.sh amd64      # 只出 amd64
#
# 构建前会先跑 scripts/sync_assets.sh 注入资产仓库的最新工具定义；
# SKIP_ASSET_SYNC=1 ./build-linux.sh 可跳过这一步（离线环境用）。
#
# Windows 上用 git-bash 跑同样可用（靠 GOOS/GOARCH 交叉编译）。
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$HERE"

# 构建前把资产仓库的最新工具定义注入 tools/opencode_tools.json。
# toolshape.go:17 用 `//go:embed tools/opencode_tools.json` 引用它，快照过期时编进
# 二进制的就是旧定义，出站工具集合只在重新构建时才变。SKIP_ASSET_SYNC=1 可显式跳过
# （离线环境、或确认快照已就位时）；跳过之外任何失败都直接终止构建，不拿旧文件凑数。
if [ "${SKIP_ASSET_SYNC:-0}" = "1" ]; then
  echo "=== sync assets: skipped (SKIP_ASSET_SYNC=1) ==="
else
  echo "=== sync assets: tools/opencode_tools.json ==="
  # 用 sh 显式调用，避免依赖 checkout 后脚本的可执行位。
  if ! sh "$HERE/scripts/sync_assets.sh"; then
    echo "=== sync assets: FAILED, build aborted (SKIP_ASSET_SYNC=1 to skip) ===" >&2
    exit 1
  fi
fi

VERSION=$(tr -d '\r\n' < VERSION)
TAG="v$VERSION"
OUT_DIR="dist"
mkdir -p "$OUT_DIR"

build_one() {
  arch="$1"
  out="$OUT_DIR/zen-direct-$TAG-linux-$arch"
  echo "=== go build linux/$arch -> $out ==="
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.appVersion=$TAG" -o "$out" .
  ls -l "$out"
}

if [ $# -ge 1 ]; then
  build_one "$1"
else
  build_one amd64
  build_one arm64
fi

echo ""
echo "version: $TAG"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$OUT_DIR"/zen-direct-"$TAG"-linux-* 2>/dev/null || true
fi
echo "BUILD OK"
