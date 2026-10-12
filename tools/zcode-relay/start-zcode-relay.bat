@echo off
rem zcode-relay：本地 start-plan 中继（消费 Zcode 免费额度通道）
rem 引擎：TriDefender/zcode-api（MIT）编译版 zcode-relay.exe，自包含无需安装 Bun/Node
rem 凭据：%USERPROFILE%\.zcode-proxy\credentials.json（inject-credential.mjs 从桌面端凭据注入 JWT）
rem 启动后 hub-go 的 Zcode 代理自动探测 8807 并切入中继（后台 5 分钟一探，重启 hub 的 Zcode 立即生效）
cd /d "%~dp0"
if not exist "%USERPROFILE%\.zcode-proxy\credentials.json" (
  echo [zcode-relay] 未发现凭据库，先注入桌面端 JWT...
  node inject-credential.mjs || (pause & exit /b 1)
)
rem 启动真浏览器铸币服务（8809，Playwright 驱动系统 Edge）
rem 2026-09-29 官方已关闭模型请求验证码校验，mint 铸币默认停用（省 400MB Edge 常驻）。
rem 若官方重新启用（relay.log 出现 3007）：去掉下面一行的 rem 即可恢复兜底。
rem start "zcode-captcha-mint" /min cmd /c "node mint\mint.mjs > mint\mint.log 2>&1"
timeout /t 2 /nobreak >nul
rem 引擎后端 auto = 优先真浏览器铸币，mint 服务不在则回退 happy-dom 模拟
set ZCODE_CAPTCHA_BACKEND=auto
set ZCODE_CAPTCHA_MINT_URL=http://127.0.0.1:8809/mint
rem 降低验证码铸币频率（防 IP 风控）：池常备 15→3、并发 3→2。
rem dsh 实际并发 1~2，小池足够；若出现冷启动变慢可调回 8/30。
set CAPTCHA_POOL_MIN=3
set CAPTCHA_POOL_MAX=12
set CAPTCHA_SOLVE_CONCURRENCY=2
echo [zcode-relay] 启动于 http://127.0.0.1:8807 ^(Ctrl+C 停止^)
zcode-relay.exe serve config.yaml
pause
