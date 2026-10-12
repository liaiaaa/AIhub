# zcode-relay — 本地 start-plan 中继（免费额度通道）

把 Zcode（Z.ai）**订阅计划的免费额度**（Start Plan 每日 + 活动赠送）接到 hub-go 代理。

```
hub-go Zcode 代理(8796) ──自动探测──> zcode-relay(8807, 本目录)
                                        │ zcode-relay.exe（TriDefender/zcode-api v4.7.0 + 本地补丁）
                                        │   · 验证码求解：auto 后端 = 优先真浏览器铸币(8809)，
                                        │     失败回退 happy-dom 模拟
                                        │   · Bearer <桌面端JWT> 认证
                                        │   · OpenAI → Anthropic 转译
                                        ▼
                              zcode.z.ai/api/v1/zcode-plan/anthropic（start-plan 网关）
                                        ▲
mint\mint.mjs(8809)：Playwright 驱动系统 Edge（真 Chromium 环境信号），
加载同一套阿里云验证码 SDK 页面，1.5~4s/枚，成功率远高于 happy-dom 模拟。
```

## 文件

| 文件 | 说明 |
|---|---|
| `zcode-relay.exe` | 引擎单文件（Bun 编译）。含本地补丁：captcha-solver 增加 `http`/`auto` 后端 |
| `mint\mint.mjs` | 真浏览器铸币服务（playwright-core + 系统 Edge，持久 profile，常驻约 250~400MB） |
| `config.yaml` | 端口 8807（仅回环）、随机本地密钥、plan: start-plan、deviceMid 与桌面端一致 |
| `inject-credential.mjs` | 桌面端 JWT 注入 + 中继本地密钥生成/同步 |
| `start-zcode-relay.bat` | 一键启动（注入凭据 → 起 mint → 起中继） |

## 使用

1. 双击 `start-zcode-relay.bat`（首次自动装 mint 依赖 + 注入凭据）。
2. hub-go 的 Zcode 代理自动探测 8807 并切入中继；`/health` 的 `relay.mode`
   变为 `start_plan_free_quota` 即接入成功。
3. 环境变量：`ZCODE_RELAY_URL=off` 强制直连；`ZCODE_CAPTCHA_BACKEND=happy`
   可退回纯模拟铸币（不跑 Edge）；`MINT_PROFILE_DIR` 指定 Edge profile 位置。

## 边界与注意

- **不静默回落**：中继挂了，代理报错提示启动中继，绝不自动退回 api.z.ai 直连——直连是充值余额计费通道，会烧真钱。
- **验证码风控风暴（已发生过，预案）**：dsh 高频使用会累积触发阿里云 IP 风控，症状是
  relay.log 出现 `mint storm detected`、请求 TTFB 从 10s 涨到 200s+ 然后连续 503、
  dsh 侧表现为「卡死数分钟不思考后报错」。处置：
  1. **重启光猫/路由器重拨换出口 IP**——IP 风控立即解除，最有效；
  2. 不方便重拨就停手中任务等冷却（几十分钟到几小时），期间别打请求（失败求解也会记入风控）；
  3. 本部署已把铸币压力降一档：`CAPTCHA_POOL_MIN=3 / MAX=12 / SOLVE_CONCURRENCY=2`
     （默认 15/60/3，池常备 token 越多向阿里云的铸币频率越高），hub-go 侧另有请求
     启动节流（`ZCODE_RELAY_START_INTERVAL_MS`，默认 2000ms，0 关闭）。
  4. 错峰使用（深夜/清晨风控明显更松）。
- 每日额度看 `/health` 的 `quota` 字段（billing/balance 直读）；Start Plan 每日 0 点重置。
- `config.yaml` 里 `claim`（自动领取活动额度）与 `mcp` 网关保持关闭（审计后收窄暴露面）。
- 引擎构建源（含验证码补丁）：`tools/zcode-relay/engine/`（TriDefender/zcode-api 克隆 + captcha-solver 补丁）。更新：`git -c http.proxy= -c https.proxy= pull` 拉上游 → **先 diff 审计变更** → `npm i && bun build --compile
  --define "require.resolve=undefined" src/index.ts --outfile <本目录>/zcode-relay.exe`。
