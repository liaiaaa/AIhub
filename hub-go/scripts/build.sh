#!/usr/bin/env bash
# ============================================================
#  AIHub (Go) - Linux 构建脚本
#
#  用法：./scripts/build.sh
#  产物：<module 目录>/bin/aihub
#
#  与 build.bat 的差异：
#    - 不加 -H=windowsgui（那是 Windows 专属链接参数，Linux 上直接报错）
#    - GOCACHE/GOPATH 默认放 ~/.cache 与 ~/.local/share，可用环境变量覆盖
#    - GOPROXY 默认走 goproxy.cn（本模块零第三方依赖，通常用不到）
# ============================================================
set -euo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # hub-go/（含 go.mod）
DATA_ROOT="$(dirname "$SRC_DIR")"                            # 数据根：state.json / 凭证目录

command -v go >/dev/null || {
  echo "[错误] 找不到 go 命令。先安装 Go >= 1.27，例如："
  echo "  wget https://npmmirror.com/mirrors/go/go1.27.0.linux-amd64.tar.gz"
  echo "  sudo tar -C /usr/local -xzf go1.27.0.linux-amd64.tar.gz"
  echo "  export PATH=\$PATH:/usr/local/go/bin"
  exit 1
}

cd "$SRC_DIR"
export GOCACHE="${GOCACHE:-$HOME/.cache/go-build}"
export GOPATH="${GOPATH:-$HOME/.local/share/go}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
mkdir -p "$GOCACHE" "$GOPATH" "$SRC_DIR/bin"

go build -trimpath -ldflags "-s -w" -o "$SRC_DIR/bin/aihub" .

echo "构建完成：$SRC_DIR/bin/aihub"
echo "数据根：  $DATA_ROOT（state.json / 凭证目录 / 日志都写在这里）"
