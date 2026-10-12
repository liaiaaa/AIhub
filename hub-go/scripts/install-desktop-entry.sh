#!/usr/bin/env bash
# ============================================================
#  AIHub (Go) - 安装桌面快捷方式（GNOME）
#
#  用法：
#    ./scripts/install-desktop-entry.sh              # 装到「应用程序菜单 + 桌面」
#    ./scripts/install-desktop-entry.sh --autostart  # 额外加「登录后自动启动」
#
#  快捷方式指向 scripts/aihub-launch.sh（编译 + 起控制台 + 拉起线路 + 开浏览器）。
#  路径按脚本自身位置推导，仓库挪动后重跑一次本脚本即可。
# ============================================================
set -euo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"   # hub-go/
REPO_ROOT="$(dirname "$SRC_DIR")"
APPS_DIR="$HOME/.local/share/applications"
ENTRY="aihub.desktop"
LABEL="AIHub 控制台"

DESKTOP_DIR="$(xdg-user-dir DESKTOP 2>/dev/null || true)"
if [ -z "${DESKTOP_DIR:-}" ] || [ ! -d "$DESKTOP_DIR" ]; then
  DESKTOP_DIR="$HOME/桌面"
  [ -d "$DESKTOP_DIR" ] || DESKTOP_DIR="$HOME/Desktop"
fi

mkdir -p "$APPS_DIR"
cat > "$APPS_DIR/$ENTRY" <<EOF
[Desktop Entry]
Type=Application
Version=1.0
Name=$LABEL
GenericName=AI 代理控制台
Comment=本地 OpenAI 兼容代理：WorkBuddy 国内/国际 · Qoder · Trae
Exec=$SRC_DIR/scripts/aihub-launch.sh
Path=$REPO_ROOT
Icon=$SRC_DIR/assets/icon.png
Terminal=false
Categories=Development;Network;Monitor;
StartupNotify=true
Keywords=aihub;proxy;workbuddy;qoder;trae;dsh;
EOF

chmod +x "$SRC_DIR/scripts/aihub-launch.sh" "$SRC_DIR/scripts/build.sh" "$SRC_DIR/scripts/start.sh"

if [ -d "$DESKTOP_DIR" ]; then
  cp "$APPS_DIR/$ENTRY" "$DESKTOP_DIR/$LABEL.desktop"
  chmod +x "$DESKTOP_DIR/$LABEL.desktop"
  # GNOME 要求桌面上的启动器「受信任」才能双击直接跑；设不了就得手动允许一次。
  gio set "$DESKTOP_DIR/$LABEL.desktop" metadata::trusted true 2>/dev/null || \
    echo "提示：首次双击若提示「未受信任」，右键选「允许启动」一次即可。"
  echo "桌面：$DESKTOP_DIR/$LABEL.desktop"
fi

if [ "${1:-}" = "--autostart" ]; then
  mkdir -p "$HOME/.config/autostart"
  cp "$APPS_DIR/$ENTRY" "$HOME/.config/autostart/$ENTRY"
  echo "登录自启：$HOME/.config/autostart/$ENTRY（取消：rm 该文件）"
fi

echo "应用菜单：$APPS_DIR/$ENTRY（Activities 里搜「AIHub」即可启动）"
