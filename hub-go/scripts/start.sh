#!/usr/bin/env bash
# ============================================================
#  AIHub (Go) - Linux 启动脚本（只启动，不重编译；编译用 build.sh）
#
#  用法：./scripts/start.sh [-port 8801] [-no-browser]
#
#  关窗口不等于退出：优先用 systemd-run --user 起一个脱离终端的服务，
#  没有 systemd 用户实例时退化为 setsid + nohup。
# ============================================================
set -euo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$SRC_DIR/bin/aihub"

[ -x "$BIN" ] || { echo "[错误] 找不到可执行文件 $BIN，请先跑 ./scripts/build.sh"; exit 1; }

echo "控制台： http://127.0.0.1:8801/（-port 可改）"

# systemd 服务不继承当前 shell 的环境，这里把会影响运行时行为的变量显式带进去：
# 代理（国际线路要 VPN/代理出网）、数据根、凭据目录覆盖、Zcode 中继开关，
# 以及几个「排障才用」的开关（2026-10-31 补：此前它们根本透不进来，
# 设了也不生效，只能靠改代码默认值）。
#   AIHUB_CHANNEL_RETRY=N     重建渠道拦截（11128）重试，N 上限 3（默认不重试）
#   AIHUB_NO_CHANNEL_RETRY=1  强制关闭上述重试（优先级更高）
#   AIHUB_KEEP_DUMPS=1        保留全部失败样本（默认只留最近 3 份）
#   AIHUB_TRAE_IDLE_SECS=N    Trae 流式空闲看门狗阈值
#   AIHUB_TRAE_EFFORT=low     Trae 思考档位覆盖（默认不注入，保持上游默认）
ENVS=()
for v in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy \
	AIHUB_ROOT WORKBUDDY_AUTH_DIR TRAE_USER_DATA_DIR QODER_APP_DATA \
	ZCODE_RELAY_URL ZCODE_RELAY_KEY \
	AIHUB_CHANNEL_RETRY AIHUB_NO_CHANNEL_RETRY AIHUB_KEEP_DUMPS \
	AIHUB_TRAE_IDLE_SECS AIHUB_TRAE_EFFORT; do
	if [ -n "${!v:-}" ]; then ENVS+=(--setenv="$v=${!v}"); fi
done
[ ${#ENVS[@]} -gt 0 ] && echo "带入环境变量：${ENVS[*]#--setenv=}"

if systemd-run --user --unit=aihub-console --collect --quiet "${ENVS[@]}" "$BIN" "$@" 2>/dev/null; then
  echo "已作为用户服务启动（aihub-console）。日志：journalctl --user -u aihub-console -f"
  echo "停止服务：systemctl --user stop aihub-console"
else
  setsid nohup "$BIN" "$@" >/dev/null 2>&1 < /dev/null &
  echo "已在后台启动，PID $!"
fi
