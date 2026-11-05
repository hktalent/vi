#!/usr/bin/env bash
# 启动 Qdrant（若未运行）和 vi 服务
set -e
cd "$(dirname "$0")"

if [ ! -x bin/vi ]; then
  go build  -trimpath -ldflags "-s -w" -o bin/vi .
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
exec ./bin/vi serve "$@"
