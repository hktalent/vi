#!/usr/bin/env bash
# 停止 vi 与 Qdrant
cd "$(dirname "$0")"
pkill -f "bin/vi" 2>/dev/null || true
pkill -f "bin/qdrant" 2>/dev/null || true
echo "stopped"
