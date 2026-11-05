#!/bin/bash
# build-importface.sh — 本地构建带 importfaces 子命令的 vi（需要 CGO + OpenCV）
# 用法: ./build-importface.sh [输出路径]
# 示例: ./build-importface.sh bin/vi
#
# 注意：构建失败也必须恢复 go.mod（用 trap 兜底），否则脏 go.mod 被提交后，
# CI 会报 "replacement directory ... does not exist"。

set -e
cd "$(dirname "$0")"

OUT="${1:-bin/vi}"

# 检查 OpenCV
if ! pkg-config --exists opencv4 2>/dev/null; then
  echo "❌ 需要 OpenCV 4: brew install opencv (macOS) / apt install libopencv-dev (Linux)"
  exit 1
fi

# 无论成功失败都恢复 go.mod（成功路径也走这里，保证幂等）
trap './scripts/restore-gocv.sh' EXIT

# 准备 patched gocv（解决 OpenCV 4.11+ 兼容性，幂等）
./scripts/patch-gocv.sh

echo "🔨 构建 importface..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  go build -tags importface -o "$OUT" .

echo "✅ 构建完成: $OUT"
echo "   运行: DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) $OUT importfaces -dir /path/to/photos"
