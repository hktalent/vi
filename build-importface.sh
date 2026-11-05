#!/bin/bash
# build-importface.sh — 本地构建带 importfaces 子命令的 vi（需要 CGO + OpenCV）
# 用法: ./build-importface.sh [输出路径]
# 示例: ./build-importface.sh bin/vi

set -e
cd "$(dirname "$0")"

OUT="${1:-bin/vi}"

# 检查 OpenCV
if ! pkg-config --exists opencv4 2>/dev/null; then
  echo "❌ 需要 OpenCV 4: brew install opencv"
  exit 1
fi

# 检查 patched gocv
if [ ! -d /tmp/gocv_patched ]; then
  echo "❌ 需要 patched gocv（解决 OpenCV 4.11 兼容性）"
  echo "   参考: cp -r \$(go env GOMODCACHE)/gocv.io/x/gocv@v0.38.0 /tmp/gocv_patched"
  echo "   然后修改 videoio.cpp（stub 掉有兼容性问题的函数）"
  exit 1
fi

echo "🔧 临时添加 gocv 依赖..."
go mod edit -require=gocv.io/x/gocv@v0.38.0 -replace=gocv.io/x/gocv=/tmp/gocv_patched

echo "🔨 构建 importface..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  go build -tags importface -o "$OUT" .

echo "🧹 恢复 go.mod..."
go mod edit -droprequire=gocv.io/x/gocv -dropreplace=gocv.io/x/gocv

echo "✅ 构建完成: $OUT"
echo "   运行: DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) $OUT importfaces -dir /path/to/photos"
