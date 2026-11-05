#!/usr/bin/env bash
# patch-gocv.sh — 准备带 videoio stub 补丁的 gocv，并配置 go.mod 的 require/replace。
# 解决 OpenCV 4.11+ 与 gocv v0.38.0 videoio.cpp 的编译兼容问题（importface 不用视频 IO，直接 stub）。
# 本地与 CI（linux/macOS/Windows Git Bash）通用。
#
# 用法: ./scripts/patch-gocv.sh [gocv版本] [补丁目录]
# 恢复: go mod edit -droprequire=gocv.io/x/gocv -dropreplace=gocv.io/x/gocv
set -euo pipefail

VERSION="${1:-v0.38.0}"
PATCH_DIR="${2:-/tmp/gocv_patched}"

cd "$(dirname "$0")/.."

# Windows Git Bash: go 是 Windows 程序，go.mod 里的路径必须是 Windows 格式
GO_PATCH_DIR="$PATCH_DIR"
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) GO_PATCH_DIR="$(cygpath -m "$PATCH_DIR")" ;;
esac

echo "==> go mod edit -require=gocv.io/x/gocv@${VERSION}"
go mod edit -require="gocv.io/x/gocv@${VERSION}"

if [ ! -f "${PATCH_DIR}/go.mod" ]; then
  echo "==> 下载 gocv ${VERSION}"
  go mod download gocv.io/x/gocv
  SRC="$(go env GOMODCACHE)/gocv.io/x/gocv@${VERSION}"
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*) SRC="$(cygpath -u "$SRC")" ;;
  esac
  echo "==> 复制 ${SRC} → ${PATCH_DIR}"
  rm -rf "${PATCH_DIR}"
  mkdir -p "$(dirname "${PATCH_DIR}")"
  cp -R "${SRC}" "${PATCH_DIR}"
  chmod -R u+w "${PATCH_DIR}"
fi

echo "==> 应用 videoio stub 补丁"
cp scripts/gocv-videoio-stub.cpp "${PATCH_DIR}/videoio.cpp"

echo "==> go mod edit -replace=gocv.io/x/gocv=${GO_PATCH_DIR}"
go mod edit -replace="gocv.io/x/gocv=${GO_PATCH_DIR}"

echo "✅ gocv 补丁就绪: ${PATCH_DIR}"
echo "   构建: go build -tags importface -o bin/vi ."
