#!/usr/bin/env bash
# zcode-relay Linux 启动（对照 start-zcode-relay.bat）：
#   1. 首次先注入桌面端 JWT（读 ~/.zcode/v2/credentials.json）
#   2. 起引擎（8807）；hub-go 的 Zcode 代理自动探测并切入免费额度通道
# 官方 2026-09-29 起模型请求不再强制验证码，mint 铸币（Playwright+Edge）不启用；
# 若 relay.log 再出现 3007，再按 tools/zcode-relay/README.md 恢复 mint。
set -e
cd "$(dirname "$0")"

if [ ! -f "$HOME/.zcode-proxy/credentials.json" ]; then
  echo "[zcode-relay] 未发现凭据库，先注入桌面端 JWT..."
  node inject-credential.mjs
fi

export ZCODE_CAPTCHA_BACKEND=auto
export CAPTCHA_POOL_MIN=3
export CAPTCHA_POOL_MAX=12
export CAPTCHA_SOLVE_CONCURRENCY=2

echo "[zcode-relay] 启动于 http://127.0.0.1:8807（Ctrl+C 停止）"
exec ./zcode-relay-linux serve config.yaml
