#!/usr/bin/env bash
# 启动 Qdrant（若未运行）和 vi 服务
# 首次运行会自动 patch gocv 以兼容 OpenCV 4.11+

pkill -f qdrant
set -e
cd "$(dirname "$0")"

# 检查 OpenCV
if ! pkg-config --exists opencv4 2>/dev/null; then
  echo "❌ 需要 OpenCV 4: brew install opencv (macOS) / apt install libopencv-dev (Linux)"
  exit 1
fi

if [ ! -x bin/vi ]; then
  echo "🔨 首次构建 vi（全功能）..."
  ./scripts/patch-gocv.sh
  DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
    go build -trimpath -ldflags "-s -w" -o bin/vi .
fi

if ! curl -s -m 2 http://127.0.0.1:6333/ > /dev/null 2>&1; then
  echo "启动 Qdrant..."
  QDRANT__STORAGE__STORAGE_PATH="$PWD/data/qdrant" \
  QDRANT__SERVICE__HTTP_PORT=6333 \
  QDRANT__SERVICE__GRPC_PORT=6334 \
  ./bin/qdrant >> data/qdrant.log 2>&1 &
  for i in $(seq 1 30); do
    curl -s -m 2 http://127.0.0.1:6333/ > /dev/null 2>&1 && break
    sleep 0.5
  done
fi

echo "vi: http://127.0.0.1:8688"
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  exec ./bin/vi serve "$@"
