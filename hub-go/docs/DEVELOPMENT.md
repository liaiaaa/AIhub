# 开发说明（Go 版）

面向接下来要在本仓库改代码的人（包括未来的你）。重点记录**结构约定**与
**容易踩的坑**，而不是重复 README 里的使用说明。

## 1. 构建与测试

Go 工具链固定在 `E:\software\go`，缓存也放 E 盘：

```powershell
cd hub-go
$env:GOCACHE='E:\software\go-cache'
$env:GOPATH ='E:\software\go-path'
& 'E:\software\go\bin\go.exe' vet  ./...
& 'E:\software\go\bin\go.exe' test ./...
```

或直接双击 `scripts\build.bat`（产物输出到 `bin\aihub.exe`）。

**离线可跑**：`go vet` 必然通过；`go test` 中带 `live` 的用例会打真实后端，
断网/无凭证时失败属正常，用 `-run` 挑选：

```powershell
& 'E:\software\go\bin\go.exe' test -run 'TestApplyThinking|TestParseQoder' ./...
```

## 2. 代码组织约定

整个程序是**单个 `main` 包**。Go 规定同一包的所有文件必须同目录，
所以「按目录分层」在这里不可行；本仓库改用**文件名前缀分组**表达职责：

| 前缀 | 职责 |
|------|------|
| `main.go` | 入口：flag、端口占用检测、路由注册 |
| `hub*.go` | 控制台编排（`hub.go`）与 HTTP API（`hub_api.go`） |
| `platform_*.go` | 平台相关（Windows 端口/进程/窗口） |
| `proxy*.go` | 通用代理核心、流聚合、工具、三个代理的配置 |
| `qoder_*.go`、`proxy_qoder.go` | Qoder 专属：凭据、模型、思考控制、流改写 |
| `auth*.go` | 凭证读取与刷新 |
| `paths.go` | 数据根解析 |
| `webui.go` | 控制台网页（内嵌） |

新增文件时**沿用前缀**，不要引入 `internal/` 子包——除非你愿意把大量内部符号导出
并逐个验证（当前共 21 个非测试源文件、约 5100 行；含测试共 30 个文件、约 6700 行，
导出成本很高）。

## 3. 关键设计决定（改代码前务必读）

### 3.1 数据根：不要再用 `os.Executable()` 直接推目录

所有落盘路径都应经由 `dataPath(...)`：

```go
log := NewFileLogger(dataPath(p.LogPath))   // ✅
log := NewFileLogger(filepath.Join(hubRoot, p.LogPath))  // ⚠️ 等价但绕过了封装
```

历史写法 `filepath.Dir(filepath.Dir(exe))` 把「数据放哪」与「exe 放哪」绑死，
exe 一移动，凭证/日志就静默写错位置。`paths.go` 的解析规则见 README。

### 3.2 慢操作必须在锁外做

`Hub.mu` 只保护字段交换，**绝不**在持锁时做绑定端口、关闭监听、读凭证文件这类慢操作。
历史事故：`statusMap()` 里读凭证，token 刷新一卡，状态轮询占住全局锁，
「停止 / 退出」全部点不动。

同理，`/api/status` 与 `/api/nick` 是分开的：前者保证永不阻塞。

### 3.3 停止代理时绝不按端口 PID 杀进程

代理跑在**本进程内**，端口上的 PID 往往就是 hub 自己。
`Stop()` 只关闭自己的 `listener`；`killPid()` 仅用于**启动前**清扫外部残留，
且有两道保险（不杀自己、不杀同名 hub 进程）。

### 3.4 端口绑定即「启动成功」的唯一判据

`Start()` 必须自己 `net.Listen` 成功才算成功，不能靠「端口能连上」猜测——
否则端口被别的进程占用时会误报已启动。

### 3.5 Qoder 不走 `AuthProvider`

Qoder 的 COSY 签名**每个请求都不同**（绑定 payload + 时间戳 + body），
套不进 `AuthProvider` 的「静态请求头」模型，所以是独立的 `QoderProxy` 类型，
对 Hub 只暴露同款的 `Start/Stop/Running`。

### 3.6 Qoder 必须用 qodercli 信封（原生思考的开关）

网关 `agent_chat_generation` 对裸 OpenAI 体会走**容错解析**：chat/model 能认，
但思考链路整体被忽略（日志常年 `think=0帧`）。必须经 `qoderEnvelope()` 转成
qodercli 形态——思考开关在**顶层 `model_config.is_reasoning`**，强度在
`parameters.reasoning_effort`，消息转 content-block 数组，`tools[].function.
parameters` 改名 `input_schema`，assistant 历史消息的 `content` 必须为字符串。
参考实现：`github.com/mo-n/dsh-provider-qoder`。改这块前先看
`qoder_envelope.go` 头注释与 `qoder_stream_test.go`。

### 3.7 Zcode：订阅额度不可反代，只有充值余额可走 API（2026-09-26 定论）

逆向 `E:\software\Zcode\resources\glm\zcode.cjs` + 真实网关探测的结论：

- 凭据解密**已打通**：`~\.zcode\v2\credentials.json` 是 `enc:v1:` AES-256-GCM，
  密钥 = `sha256("zcode-credential-fallback:win32:{homedir}:{username}")`，
  取 `*:coding-plan:*:api-key` 条目即可作 `api.z.ai` 的 Bearer（已验证 key 有效）。
- 但 **ZCode Start Plan 的每日 token 额度**走 `zcode.z.ai/api/v1/zcode-plan/*`
  私有中继，强制三个请求头：`x-aliyun-captcha-verify-param`（阿里云验证码，
  一次性）、`x-client-sig`、`x-client-pow`（设备签名/工作量证明）。
  裸 POST 该路径回 `3007 captcha verify failed`。**第三方无法合法复现，别再试。**
- 所以本代理走 `api.z.ai/api/coding/paas/v4`（官方 API Key 直连，充值余额计费）。
  key 有效但无余额时上游回 `1113 Insufficient balance`——代理会把它转成
  带指引的 OpenAI error（见 `zcodeHintError`）。
- 探测用的临时脚本与载荷样本在 `archive\probes-zcode-20260926\`。

### 3.7.1 Zcode 借鉴 zcode-api 的三项接入（2026-09-27 定论，真机验证）

对照开源工程 TriDefender/zcode-api（本地缓存 `.ref-zcode-api\src\`）逐文件精读 +
探针实测后，落地三件事（**验证码求解器明确不移植**：风控对抗、风险落在账号上，
其 3007 墙经实测确认仍然有效）：

- **桌面端身份头**（`zcode_identity.go`）：对齐官方客户端两套 header 组——
  LLM 补全面 `g6n`（UA `ZCode/<ver>`、`X-ZCode-Agent: glm`、`X-Title: Z Code@cli`、
  `X-Os-Category/Os-Version` 等，**无** `X-Device-Mid`）与控制面 `TV`（去
  `X-ZCode-Agent`、加 `X-Device-Mid`）。值来源：版本扫
  `~\.zcode\v2\runtime\provider\<平台>\<semver>`、设备号读
  `telemetry-state.json`、OS 版本读注册表 CurrentBuildNumber（stdlib 无
  `os.KernelVersion`）。官方 LLM 流量本就落在本代理同款 openai:chat 通道，
  补齐后指纹层与官方一致。
- **实时模型目录**（`zcode_catalog_refresh.go`）：`GET zcode.z.ai/api/v1/
  client/configs?app_version=&platform=` 公开可读（实测 200），builtinModels +
  providers[].models 合并内置快照（远端为主、快照补「思考不可关闭」等文档
  结论），成功 TTL 30min、失败保留旧目录 + 30s 冷却。
- **端点路由**（`zcode_routing.go`）：`GET /api/v1/agent/configs` 下发
  `data.proxyEndpoint.mapping`（from→to，服务端随时可改写端点，2026-09-27
  实测当期为空）。完整 URL 精确匹配 + base 前缀匹配两段式，严格 fail-open
  （3s 超时、失败冷却 30s、解析异常弃用整表）。
- **顺带打通**：凭据选取确定化（`individual-coding-plan` 族优先，map 遍历
  无序曾导致重启换 key）；提取 `zcodejwttoken`，`billing/balance`（JWT +
  `X-Device-Mid` + `x-coding-plan-api-key` 三头，实测 200）把套餐余额接到
  `/health` 的 `quota` 字段。
- 真机 smoke：`ZCODE_LIVE_TEST=1 go test -run TestZcodeLiveReverse`。

### 3.7.2 Zcode 免费额度通道：本地 start-plan 中继（2026-09-27 定论，真机验证）

验证码求解器**不自研**，但接入了 MIT 开源引擎 TriDefender/zcode-api（v4.7.0），
把 Start Plan 免费额度真正接进了代理（部署在 `tools\zcode-relay\`）：

- **引擎形态**：zcode-api 是 Bun 工程，其验证码求解器（happy-dom 进程内求解
  阿里云无痕验证，实测 2s/枚 T001 通过）**只在 Bun 运行时下可用**——Node +
  esbuild CJS bundle 会 `window is not defined`（脚本 eval 全局不可见，钉
  happy-dom 版本也无效）。因此用 `bun build --compile` 出自包含 exe（88MB）。
- **凭据注入**：`tools\zcode-relay\inject-credential.mjs` 把桌面端
  `zcodejwttoken` 按 zcode-api 的凭据库格式（AES-256-GCM，种子
  `{homedir}-{platform}-{arch}`，信封 `base64(iv||ct||tag)`）写入
  `~/.zcode-proxy/credentials.json`，免浏览器 OAuth。JWT 无 exp 声明不过期。
- **接线**：hub-go 侧 `zcode_relay.go` 自动探测 `127.0.0.1:8807/v1/models`
  （Start 时 + 后台 5 分钟重探；`ZCODE_RELAY_URL`/`ZCODE_RELAY_KEY` 可覆盖，
  `off` 强制关闭），命中后上游切到中继的 `/v1/chat/completions`（OpenAI
  兼容，Bearer `hub-zcode-relay`）。
- **不静默回落**：中继不可达时直接报错带指引，绝不退回 api.z.ai 直连——
  直连是计费通道，静默回落会烧真钱。
- **实测**：代理→中继→start-plan 网关 200，GLM-5.3 真实回复，额度被真实
  消费（billing/balance 同步可见扣减）。3007 墙由引擎求解器自动过。
- 配置默认 `claim`（自动领活动额度）关闭；引擎源码与重建命令见
  `tools\zcode-relay\README.md`。

### 3.8 Qoder 排队帧 10605：不是鉴权错误，按队列语义处理（2026-09-26 定论）

晚高峰网关会返回 HTTP 200 + 业务错误帧：外层 `code:"403"`，message 里嵌两层
JSON，最内层是 `isQueued/queueCount/queueType:"p3"/retryAfterSeconds/
serviceAvailable/waitTime`（样本见 `qoder_queue.go` 头注释）。曾因此被 DSH
显示成「API 密钥无效」——**凭据是好的，纯粹是网关排队**。处理定论：

- 识别与重试在 `qoder_queue.go`：按 `retryAfterSeconds` 真实等待重试，
  总预算默认 240s（`QODER_QUEUE_MAX_WAIT_SECS` 可调，0 关闭）；HTTP 403 +
  10605 体与流中途排队帧也兜底。
- 放弃/透传时的文案必须是「上游排队中（429）…」语义，**不得含 403/密钥
  字样**（客户端按 403 分类成 AUTH 会再次误导）。
- 流式路径（含降级流）在排队等待期发**空 delta 心跳帧**保活，避免客户端
  静默超时掐掉本来能成的请求；心跳帧形态与网关自带空 usage 帧一致。
- 非流式 JSON 路径无法心跳，靠 300s ctx + 预算 240s < 常见静默超时兜底。

## 4. 测试约定

- 需要真实凭证/网络的测试要**可跳过**（`t.Skip`），不要用 `t.Fatal` 让离线环境红一片。
- 不要硬编码绝对路径。要定位仓库根，用 `repoRoot(t)`（见 `proxy_intl_test.go`），
  它由测试文件自身位置推导，仓库换盘符/层级都不受影响。

  > 历史教训：`proxy_intl_test.go` 曾写死 `D:\C\Desktop\workbuddy-hub`，
  > 目录改名后测试长期失败。

- 纯函数（思考控制、模型解析、流改写）应尽量多加表驱动用例，
  这类逻辑最容易在重构中悄悄改变行为。

## 5. 重构安全做法（本仓库已验证有效）

大文件拆分时用「**按行区间搬运 + 事后校验**」：

1. 先 `git commit` 一个干净基线，并把原文件另存到 `archive\src-backups\`；
2. 按既有 `// ---------- x ----------` 分节边界切分；
3. 各文件**重算 import**（Go 的 import 是文件级的，搬运后必然需要调整）；
4. 用脚本对比「拆分前的 `func`/`type` 列表」与「拆分后所有文件合并的列表」，
   数量与签名必须完全一致；
5. `go vet` + `go test` + 用产物在**空闲端口**跑一次端到端
   （`bin\aihub.exe -port 8899 -no-browser`，再 `curl /api/status`）。

本仓库 `proxy_qoder.go`（1642 行）、`proxy.go`（884 行）、`main.go`（640 行）
都是按此法拆分并校验的，详见 git log。

## 6. 常见坑

### ⚠️ 头号大坑：`go build` 不带 `-o` 会覆盖正在用的 exe

`go build` 不带 `-o` 时，会在**当前目录**生成以包名命名的可执行文件。
本模块 `module aihub` 且 main 包在模块根，于是：

```powershell
cd hub-go
& 'E:\software\go\bin\go.exe' build ./...     # ❌ 直接覆盖 hub-go\aihub.exe！
```

这会**静默替换掉你正在使用的那份 exe**（虽然正在运行的进程用的是内存映像、
不会立刻崩，但下次启动就跑的是新文件了）。

**永远显式指定输出路径**：

```powershell
& 'E:\software\go\bin\go.exe' build -o bin\aihub.exe .   # ✅
```

或者直接用 `scripts\build.bat`（它固定 `-o bin\aihub.exe`，安全）。

> 本仓库确实发生过一次：重构期间用 `go build ./...` 验证时覆盖了 `hub-go\workbuddy-hub.exe`。
> 原始文件（11575296 字节）已备份到
> `archive\exe-backups\workbuddy-hub-ORIGINAL-11575296.exe`，需要回退可从此处取。

### ⚠️ 二号大坑：`.bat` 必须是 ASCII + CRLF

中文 Windows 的 `cmd.exe` 按 **GBK** 读取 `.bat` 文件。如果脚本写成 **UTF-8**（尤其是
带中文注释/回显），cmd 会把多字节序列解析错乱，出现这类报错：

```
'��动打开控制台' is not recognized as an internal or external command
'The system cannot find the file \bin\workbuddy-hub.exe.'
```

脚本会**直接跑不起来**（或只跑一半）。同理，`.bat` 必须是 **CRLF** 换行，裸 LF 也会出问题。

**约定**：`scripts\*.bat` 一律 **纯 ASCII 内容 + CRLF**（消息用英文，和原项目一致）。

> 本仓库实际踩过：重构时把三个 `.bat` 改成 UTF-8 中文注释 + LF，
> 结果 `build.bat` / `启动控制台-Go.bat` / `编译并启动.bat` 全部失效
> （桌面快捷方式点了没反应）。已全部改回 ASCII + CRLF 并逐个实测。

### 数据根解析：只用权威标志

`looksLikeDataRoot` 只认 `state.json` 和凭证目录，**不要**把 `*.log` 加回标志列表：
日志是 hub 自己写的，一旦某目录被误写日志就会被误判成数据根，错误会自我固化。

解析会**从 exe 目录逐级向上找最多 4 级**，所以 exe 放在 `bin\` 也能找到仓库根。
改 `paths.go` 后务必跑这两个回归测试：

```
TestLogsAloneDoNotMakeDataRoot
TestResolveHubRootFindsRootAboveBin
```

### 其它

| 现象 | 原因 / 处理 |
|------|-------------|
| 改了代码但行为没变 | 正在跑的是**旧 exe**。重新 `build.bat` 并重启。 |
| 「国际代理起不来」 | 先看凭证目录有没有 `.info`；再看是否需要 VPN。 |
| 端口被占启动失败 | 程序会自动清扫并重试 4 次；仍失败看是否被别的程序占用。 |
| 二进制体积异常 | 用 `build.bat`（含 `-s -w -H=windowsgui`）；手动 `go build` 不带这些标志会更大、还会弹黑窗。 |
| 测试里 `resolveHubRoot()` 指向临时目录 | 正常：`go test` 的「exe」是测试二进制。生产代码不受影响。 |
| 日志写到了意料外的目录 | 检查该目录是否含 `state.json`/凭证目录（会被当数据根）；或设 `AIHUB_ROOT` 固定。 |

## 7. 归档区

`..\archive\` 保存了重构期间清理出来、但**没有删除**的东西：

| 子目录 | 内容 |
|--------|------|
| `exe-backups\` | 12 个旧 exe 备份（约 108MB，含重构前的 `workbuddy-hub-ORIGINAL-11575296.exe`，确认无用时自行删除） |
| `src-backups\` | 拆分的源文件原名（对照校验用）、`.bak`、`_backups_disabled\` |
| `scripts\` | 一次性调试脚本 `_*.ps1` |
| `logs\` | 历史日志（含期间误写到 `hub-go\` 的两份） |
| `python-legacy\` | 已失效的 Python 版 `启动控制台.bat` |

Git 基线快照：`chore: 重构前快照（基线）` 提交，可用它对比重构前后差异。
