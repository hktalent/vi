#!/bin/bash
# impt.sh — 完整构建 vi 并执行 importfaces 子命令
# 用法: ./impt.sh <图片/视频目录> [标签]
# 示例: ./impt.sh ~/Photos/婚礼 "婚礼"
#       VI_VIDEO=1 ./impt.sh /Volumes/Home/MyPhoto "手机时光"     # 连视频(MOV/MP4)一起导入
#       VI_VIDEO=1 VI_VSTEP=2 VI_VMAX=300 ./impt.sh <目录>        # 每 2 秒抽 1 帧，每视频最多 300 帧
#       VI_VIDEO=1 VI_VPLATES=0 ./impt.sh <目录>                  # 视频只导人脸，不导车牌

set -e
cd "$(dirname "$0")"

# 检查 OpenCV
if ! pkg-config --exists opencv4 2>/dev/null; then
  echo "❌ 需要 OpenCV 4: brew install opencv (macOS) / apt install libopencv-dev (Linux)"
  exit 1
fi

LABEL="${2:-}"

# patch gocv（幂等）
./scripts/patch-gocv.sh

echo "🔨 编译 vi（全功能，含 importfaces）..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  go build -trimpath -o ./bin/vi -ldflags "-s -w" .

echo "✅ 编译完成: ./bin/vi"

if [ -z "$1" ]; then
  echo ""
  echo "用法: ./impt.sh <图片/视频目录> [标签]"
  echo "示例: ./impt.sh ~/Photos/婚礼 婚礼"
  echo "      VI_VIDEO=1 ./impt.sh ~/Movies 视频集   # 导入视频中的人脸/车牌"
  exit 0
fi

VIDEO_ARGS=""
if [ "${VI_VIDEO:-0}" = "1" ]; then
  VIDEO_ARGS="-video -video-step ${VI_VSTEP:-1} -video-max ${VI_VMAX:-200}"
  [ "${VI_VFACES:-1}" = "0" ] && VIDEO_ARGS="$VIDEO_ARGS -video-faces=false"
  [ "${VI_VPLATES:-1}" = "0" ] && VIDEO_ARGS="$VIDEO_ARGS -video-plates=false"
  echo "🎬 视频导入已启用: step=${VI_VSTEP:-1}s max=${VI_VMAX:-200}帧"
fi

echo ""
echo "📥 开始导入..."
DYLD_LIBRARY_PATH=$(pkg-config --variable=libdir opencv4 2>/dev/null || echo /usr/local/opt/opencv/lib) \
  ./bin/vi importfaces -dir "$1" ${LABEL:+-label "$LABEL"} $VIDEO_ARGS
