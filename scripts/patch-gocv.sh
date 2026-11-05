#!/usr/bin/env bash
# patch-gocv.sh — 准备带 videoio stub 补丁的 gocv，并配置 go.mod 的 require/replace。
# 解决 OpenCV 4.11+ 与 gocv v0.38.0 videoio.cpp 的编译兼容问题（importface 不用视频 IO，直接 stub）。
#
# 设计要点（勿随意改动）：
#  1) 补丁目录固定在仓库内 `.build/gocv`（已被 .gitignore 忽略），不再用 /tmp。
#     /tmp 会被系统清理、跨机器不存在；一旦把绝对路径写进 go.mod 并提交，CI 必然报
#     "replacement directory /tmp/gocv_patched does not exist"。
#  2) go.mod 里写的是**相对路径** `./.build/gocv`，本机 / Linux / macOS / Windows 通用。
#  3) 本脚本执行完会**保留** go.mod 的 require+replace（后续 go build 需要它）；
#     构建结束后必须调用 scripts/restore-gocv.sh 恢复，否则脏 go.mod 会被误提交。
#
# 用法: ./scripts/patch-gocv.sh [gocv版本] [补丁目录(相对仓库根)]
# 恢复: ./scripts/restore-gocv.sh
set -euo pipefail

VERSION="${1:-v0.38.0}"
PATCH_DIR="${2:-.build/gocv}"

cd "$(dirname "$0")/.."
ROOT="$PWD"

# go.mod 里统一用 ./ 开头的相对路径
GO_PATCH_DIR="./${PATCH_DIR#./}"
ABS_PATCH_DIR="${ROOT}/${PATCH_DIR#./}"

echo "==> go mod edit -require=gocv.io/x/gocv@${VERSION}"
go mod edit -require="gocv.io/x/gocv@${VERSION}"

if [ ! -f "${ABS_PATCH_DIR}/go.mod" ]; then
  echo "==> 下载 gocv ${VERSION}"
  go mod download gocv.io/x/gocv
  SRC="$(go env GOMODCACHE)/gocv.io/x/gocv@${VERSION}"
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) SRC="$(cygpath -u "$SRC")" ;;
  esac
  if [ ! -d "$SRC" ]; then
    echo "❌ 模块缓存中找不到 $SRC" >&2
    exit 1
  fi
  echo "==> 复制 ${SRC} → ${ABS_PATCH_DIR}"
  rm -rf "${ABS_PATCH_DIR}"
  mkdir -p "$(dirname "${ABS_PATCH_DIR}")"
  cp -R "${SRC}" "${ABS_PATCH_DIR}"
  chmod -R u+w "${ABS_PATCH_DIR}" 2>/dev/null || true
fi

echo "==> 应用 videoio stub 补丁"
cp scripts/gocv-videoio-stub.cpp "${ABS_PATCH_DIR}/videoio.cpp"

echo "==> go mod edit -replace=gocv.io/x/gocv=${GO_PATCH_DIR}"
go mod edit -replace="gocv.io/x/gocv=${GO_PATCH_DIR}"

echo "✅ gocv 补丁就绪: ${ABS_PATCH_DIR} (go.mod replace → ${GO_PATCH_DIR})"
echo "   构建: go build -tags importface -o bin/vi ."
echo "   收尾: ./scripts/restore-gocv.sh"
