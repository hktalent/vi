#!/bin/bash
# build-importface.sh — 本地构建带 importfaces 子命令的 vi（需要 CGO + OpenCV）
# 用法: ./build-importface.sh [输出路径]
# 示例: ./build-importface.sh bin/vi

set -e
cd "$(dirname "$0")"

OUT="${1:-bin/vi}"

# 检查 OpenCV
if ! pkg-config --exists opencv4 2>/dev/null; then
  echo "❌ 需要 OpenCV 4: brew install opencv (macOS) / apt install libopencv-dev (Linux)"
  exit 1
fi

# 准备 patched gocv（解决 OpenCV 4.11+ 兼容性，幂等）
./scripts/patch-gocv.sh

echo "🔨 构建 importface..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  go build -tags importface -o "$OUT" .

echo "🧹 恢复 go.mod..."
go mod edit -droprequire=gocv.io/x/gocv -dropreplace=gocv.io/x/gocv

echo "✅ 构建完成: $OUT"
echo "   运行: DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) $OUT importfaces -dir /path/to/photos"
