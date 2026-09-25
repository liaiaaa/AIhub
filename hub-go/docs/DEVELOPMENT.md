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

或直接双击 `scripts\build.bat`（产物输出到 `bin\workbuddy-hub.exe`）。

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
   （`bin\workbuddy-hub.exe -port 8899 -no-browser`，再 `curl /api/status`）。

本仓库 `proxy_qoder.go`（1642 行）、`proxy.go`（884 行）、`main.go`（640 行）
都是按此法拆分并校验的，详见 git log。

## 6. 常见坑

| 现象 | 原因 / 处理 |
|------|-------------|
| 改了代码但行为没变 | 正在跑的是**旧 exe**。重新 `build.bat` 并替换后重启。 |
| 「国际代理起不来」 | 先看凭证目录有没有 `.info`；再看是否需要 VPN。 |
| 端口被占启动失败 | 程序会自动清扫并重试 4 次；仍失败看是否被别的程序占用。 |
| 二进制体积异常 | 用 `build.bat`（含 `-s -w -H=windowsgui`）；手动 `go build` 不带这些标志会更大。 |
| 测试里 `resolveHubRoot()` 指向临时目录 | 正常：`go test` 的「exe」是测试二进制。生产代码不受影响。 |

## 7. 归档区

`..\archive\` 保存了重构期间清理出来、但**没有删除**的东西：

| 子目录 | 内容 |
|--------|------|
| `exe-backups\` | 11 个旧 exe 备份（约 80MB，可确认无用时自行删除） |
| `src-backups\` | 拆分的源文件原名（对照校验用）、`.bak`、`_backups_disabled\` |
| `scripts\` | 一次性调试脚本 `_*.ps1` |
| `logs\` | 历史日志 |
| `python-legacy\` | 已失效的 Python 版 `启动控制台.bat` |

Git 基线快照：`chore: 重构前快照（基线）` 提交，可用它对比重构前后差异。
