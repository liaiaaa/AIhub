#!/usr/bin/env bash
# ============================================================
#  AIHub (Go) - 安全切换到新编译的产物，并做端到端自检
#
#  一步做完：记录当前在跑的线路 → 停旧实例 → 换上 bin/aihub.new →
#  按原样拉起线路和聚合口 → 用「并行工具调用 + 图片压缩提示」这个真实
#  形状打一次 :8790，确认上游 11148 已经不再复现。
#
#  用法：
#    bash scripts/aihub-switch.sh              # 自动恢复切换前在跑的线路
#    AIHUB_LINES="intl" bash scripts/aihub-switch.sh
#
#  产物从哪来：源码目录里
#    go build -trimpath -ldflags "-s -w" -o bin/aihub.new .
#  回滚：bin/aihub.prev 是切换前的产物，cp 回 bin/aihub 再跑一次本脚本即可。
# ============================================================
set -u

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$SRC_DIR" || exit 1
NEW=bin/aihub.new

if [ ! -f "$NEW" ]; then
  echo "✗ 缺少 $NEW。先在 $SRC_DIR 编译："
  echo "    go build -trimpath -ldflags \"-s -w\" -o bin/aihub.new ."
  exit 1
fi

# 探测某端口是否在听。
# 不能只信 ss 的 `sport = :PORT` 过滤器：在受限/精简环境里它可能整体失败并返回空
# （不是「这个端口没在听」，而是「什么都查不到」），于是下面的线路探测全判否，
# AIHUB_LINES 变空 → aihub-launch 一条都不拉起（2026-10-17 实测踩过：切完 oc 掉了，
# 而 dsh 的 aihub-oc provider 正连在那上面）。所以 ss 说没在听时，再用 TCP 连接
# 兜一次 —— 这条路不依赖 ss 的过滤器语法，也不需要特权。
listening() {
  ss -ltnH "sport = :$1" 2>/dev/null | grep -q . && return 0
  timeout 1 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1" 2>/dev/null
}

# 1) 记下切换前哪些线路在监听，切完按原样恢复（尤其不能漏掉 dsh 在用的 zcode）。
# 8800:oc 也必须探——dsh 的 aihub-oc provider 直连该端口，漏了会让那些模型莫名失联。
LINES=""
for pair in "8787:dom" "8799:intl" "8797:qoder" "8795:trae" "8796:zcode" "8800:oc"; do
  port="${pair%%:*}"; name="${pair##*:}"
  listening "$port" && LINES="$LINES $name"
done
AGG_WAS=0; listening 8790 && AGG_WAS=1
LINES="${AIHUB_LINES:-${LINES# }}"
# 老进程在跑、却一条线路都没探到 = 探测本身出问题的典型形状。这时静默继续会把
# 正在用的线路全丢掉，必须吵出来让人看见。
OLD_RUNNING="$(pgrep -x aihub || true)"
if [ -z "$LINES" ] && [ -n "$OLD_RUNNING" ]; then
  echo "⚠ 探测不到任何在听的线路，但旧实例还在跑（pid $OLD_RUNNING）——多半是 ss 端口过滤器不可用。"
  echo "  为避免把你在用的线路弄丢，这里按「全部已知线路」恢复；如不对请用"
  echo "  AIHUB_LINES=\"dom intl ...\" 显式指定后重跑。"
  LINES="dom intl qoder trae zcode oc"
fi
echo "· 切换前：线路[${LINES:-（无）}] 聚合口[$AGG_WAS]"

# 2) 停旧实例。确认退干净了才动产物，避免出现两个实例抢端口。
PIDS="$(pgrep -x aihub || true)"
if [ -n "$PIDS" ]; then
  echo "· 停止旧实例 pid=$PIDS"
  kill $PIDS
  for _ in $(seq 30); do pgrep -x aihub >/dev/null || break; sleep 0.3; done
  if pgrep -x aihub >/dev/null; then
    echo "✗ 旧实例仍存活（$PIDS），已中止，产物未改动"
    exit 1
  fi
fi

# 3) 换产物（旧的那份留一份做回滚）。
[ -f bin/aihub ] && cp -f bin/aihub bin/aihub.prev
mv -f "$NEW" bin/aihub

# 4) 起控制台 + 拉起线路。AIHUB_NO_BUILD=1 保证跑的就是刚换上的这份。
echo "· 启动新实例"
AIHUB_NO_BUILD=1 AIHUB_NO_BROWSER=1 AIHUB_LINES="$LINES" scripts/aihub-launch.sh

# 5) 聚合口 + 端到端自检。
python3 - "${AGG_WAS}" <<'PY'
import json, sys, time, urllib.request, urllib.error

agg_was = sys.argv[1] == "1"
HUB, AGG = 8801, 8790
SLUG = "intl.deepseek-v4.1-flash"


def call(port, path, method="GET", payload=None, timeout=30):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}{path}", method=method, data=data,
        headers={"content-type": "application/json",
                 "authorization": "Bearer local-hub"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status, r.read().decode("utf-8", "replace")


def up(port, path="/v1/models"):
    try:
        call(port, path, timeout=5)
        return True
    except Exception:
        return False


for _ in range(40):
    if up(HUB, "/api/status"):
        break
    time.sleep(0.5)
else:
    print("✗ 控制台 :%d 没起来，去终端看 aihub 输出" % HUB)
    sys.exit(1)

if not up(AGG):
    if agg_was:
        print("· 聚合口未在监听，恢复 :8790")
        call(HUB, "/api/agg/start", "POST", {})
        for _ in range(20):
            if up(AGG):
                break
            time.sleep(0.3)

if not up(AGG):
    print("△ 聚合口 :8790 仍未监听（控制台「Codex 聚合线路」点一下启动）")
    sys.exit(0)

# 11148 复现形状：assistant 并行两次工具调用 → tool 结果里带图，两条结果中间
# 夹一条 <image_resize_notice> 的 system。修复前上游 400 code 11148。
PNG = ("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAf"
       "FcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
notice = lambda px: ("<image_resize_notice>\nImage 1 of 1 in the preceding tool output "
                     "was resized from 2160x%d to 2048x%d pixels.\n</image_resize_notice>" % (px, px - 49))
body = {
    "model": SLUG,
    "stream": False,
    "instructions": "You are a helpful coding agent.",
    # 与 Codex 一致：只给 summary，不带 effort（否则 hub 注入最高档、白等几分钟）
    "reasoning": {"summary": "auto"},
    "max_output_tokens": 16,
    "tools": [{"type": "function", "name": "view_image",
               "parameters": {"type": "object", "properties": {}, "additionalProperties": False}}],
    "input": [
        {"type": "message", "role": "user",
         "content": [{"type": "input_text", "text": "看这两张图"}]},
        {"type": "function_call", "name": "view_image", "call_id": "call_00_a", "arguments": "{}"},
        {"type": "function_call", "name": "view_image", "call_id": "call_01_b", "arguments": "{}"},
        {"type": "function_call_output", "call_id": "call_00_a",
         "output": [{"type": "input_image", "image_url": PNG}]},
        {"type": "message", "role": "system",
         "content": [{"type": "input_text", "text": notice(950)}]},
        {"type": "function_call_output", "call_id": "call_01_b",
         "output": [{"type": "input_image", "image_url": PNG}]},
        {"type": "message", "role": "system",
         "content": [{"type": "input_text", "text": notice(820)}]},
        {"type": "message", "role": "user",
         "content": [{"type": "input_text", "text": "只回一个字"}]},
    ],
}

t0 = time.time()
try:
    code, raw = call(AGG, "/v1/responses", "POST", body, timeout=120)
    ok = code == 200
except urllib.error.HTTPError as e:
    code, raw, ok = e.code, e.read().decode("utf-8", "replace"), False
except Exception as e:
    print("✗ 自检请求没发出去：%s: %s" % (type(e).__name__, e))
    sys.exit(1)

tag = "11148" in raw or "tool calls and tool results" in raw
if ok:
    print("✓ 自检通过：并行工具调用 + 图片压缩提示插队，%.1fs 内正常返回" % (time.time() - t0))
else:
    print("✗ 自检失败 HTTP %s%s：%s" % (code, "（仍是 11148）" if tag else "", raw[:400]))
PY

echo "· 现在监听：$(ss -ltnH 2>/dev/null | grep -oE '127\.0\.0\.1:(8787|8790|8795|8796|8797|8799|8800|8801)' | sort -u | tr '\n' ' ')"
