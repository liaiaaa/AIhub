#!/usr/bin/env bash
# ============================================================
#  AIHub (Go) - Linux 桌面快捷启动
#
#  一步做完：编译（有 go 就编）→ 起控制台 → 拉起常用线路 → 开浏览器。
#  对应 Windows 上「双击 AIHub 快捷方式 = 编译 + 启动」。
#
#  可选环境变量：
#    AIHUB_LINES       要自动启动的线路。默认空 = 全部停止（2026-10-10 用户要求：
#                      启动后所有端口关闭，去控制台手动拉起需要的线路）。
#                      要恢复自动拉起可 AIHUB_LINES="dom intl qoder trae" 启动。
#    AIHUB_NO_BUILD=1  跳过编译，直接用现有产物
#    AIHUB_NO_BROWSER=1 不打开浏览器
#    AIHUB_PORT        控制台端口，默认 8801
# ============================================================
set -u

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$SRC_DIR/bin/aihub"
PORT="${AIHUB_PORT:-8801}"
LINES="${AIHUB_LINES:-}"

notify() {
  command -v notify-send >/dev/null 2>&1 && notify-send "AIHub" "$1"
  echo "[AIHub] $1"
}

# 1) 编译。.desktop 启动不读 ~/.bashrc，所以显式补上 go 的安装位置。
export PATH="$PATH:/usr/local/go/bin"
if [ "${AIHUB_NO_BUILD:-0}" != "1" ] && command -v go >/dev/null 2>&1; then
  if ! (cd "$SRC_DIR" && go build -trimpath -ldflags "-s -w" -o "$BIN" . >/tmp/aihub-build.log 2>&1); then
    notify "编译失败，沿用旧产物（详见 /tmp/aihub-build.log）"
  fi
fi
if [ ! -x "$BIN" ]; then
  notify "找不到可执行文件 $BIN，请先跑 scripts/build.sh"
  exit 1
fi

# 2) 起控制台。已在跑就复用（不重复起，避免第二个实例去清扫端口）。
if ! ss -tlnH "sport = :$PORT" 2>/dev/null | grep -q .; then
  if ! "$SRC_DIR/scripts/start.sh" -no-browser >/dev/null 2>&1; then
    notify "控制台启动失败"
    exit 1
  fi
fi

# 3) 等就绪 + 拉起线路。本机没有 curl，用 python3 标准库发这几个本地请求。
SUMMARY="$(python3 - "$PORT" "$LINES" <<'PY'
import json, sys, time, urllib.request

port, lines = sys.argv[1], sys.argv[2].split()


def call(path, method="GET"):
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}", method=method,
        data=b"{}" if method == "POST" else None,
        headers={"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=25) as r:
        return json.loads(r.read().decode())


for _ in range(40):  # 最多等 20 秒
    try:
        call("/api/status")
        break
    except Exception:
        time.sleep(0.5)
else:
    print("控制台未就绪")
    sys.exit(1)

out = []
for ln in lines:
    try:
        r = call(f"/api/{ln}/start", "POST")
        out.append(f"{ln}={'✓' if r.get('ok') else '✗ ' + str(r.get('msg'))[:46]}")
    except Exception as e:
        out.append(f"{ln}=✗ {type(e).__name__}")
print(" ".join(out))
PY
)"
notify "控制台 :$PORT · $SUMMARY"

# 4) 开浏览器（二进制那侧已经用 -no-browser 压住了，这里只开一次）
if [ "${AIHUB_NO_BROWSER:-0}" != "1" ] && command -v xdg-open >/dev/null 2>&1; then
  xdg-open "http://127.0.0.1:$PORT/" >/dev/null 2>&1 &
fi
