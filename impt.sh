#!/bin/bash
# impt.sh — 完整构建 vi 并执行 importfaces 子命令
# 用法: ./impt.sh <图片目录> [标签]
# 示例: ./impt.sh ~/Photos/婚礼 "婚礼"

set -e
cd "$(dirname "$0")"

# 检查 OpenCV
if ! pkg-config --exists opencv4 2>/dev/null; then
  echo "❌ 需要 OpenCV 4: brew install opencv (macOS) / apt install libopencv-dev (Linux)"
  exit 1
fi

OUT="${2:-}"
LABEL="${3:-}"

# patch gocv（幂等）
./scripts/patch-gocv.sh

echo "🔨 编译 vi（全功能，含 importfaces）..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  go build -trimpath -o ./bin/vi -ldflags "-s -w" .

echo "✅ 编译完成: ./bin/vi"

if [ -z "$1" ]; then
  echo ""
  echo "用法: ./impt.sh <图片目录> [标签]"
  echo "示例: ./impt.sh ~/Photos/婚礼 婚礼"
  exit 0
fi

echo ""
echo "📥 开始导入图片..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  ./bin/vi importfaces -dir "$1" ${LABEL:+-label "$LABEL"}
