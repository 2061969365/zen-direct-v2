#!/bin/sh
# build-linux.sh — 交叉/本机编译 Linux 版本（amd64 + arm64，静态、无 cgo）。
#
# 用法：
#   ./build-linux.sh            # 两个架构都出，产物落 dist/
#   ./build-linux.sh amd64      # 只出 amd64
#
# Windows 上用 git-bash 跑同样可用（靠 GOOS/GOARCH 交叉编译）。
set -eu

HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$HERE"

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
