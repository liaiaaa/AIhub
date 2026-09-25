# WorkBuddy Hub

把 WorkBuddy 系列的四个后端（国内 / 国际 / MiMo / Qoder）统一转成本地 **OpenAI 兼容**
接口的 Go 单文件程序。桌面控制台一键启停，代理全部跑在同一个进程里。

```
浏览器控制台  http://127.0.0.1:8801
      │
      └─ workbuddy-hub.exe（单进程）
           ├─ 国内  127.0.0.1:8787 → codebuddy 国内
           ├─ 国际  127.0.0.1:8799 → www.workbuddy.ai
           ├─ MiMo  127.0.0.1:8798 → 小米 SSO
           └─ Qoder 127.0.0.1:8797 → gateway.qoder.com.cn
```

## 快速开始

| 步骤 | 操作 |
|------|------|
| 编译 | 双击 `hub-go\scripts\build.bat` → 产物在 `hub-go\bin\workbuddy-hub.exe` |
| 启动 | 双击 `hub-go\scripts\启动控制台-Go.bat`，浏览器自动打开控制台 |
| 控制 | 网页上手动点「启动 / 停止」；底部可「全部停止 / 退出」 |

编译需要用 `E:\software\go\bin\go.exe`（缓存也放 E 盘，不占 C 盘）。

> 启动默认**全部停止**：程序会先清扫 8787/8797/8798/8799 的残留监听，
> 再由你手动选择开启哪一个。

## 四个代理

| 代理 | 端口 | 后端 | 认证方式 |
|------|------|------|----------|
| 国内 | 8787 | codebuddy 国内 | 凭证文件（直连） |
| 国际 | 8799 | `www.workbuddy.ai` | 凭证目录（读系统代理，需 VPN） |
| MiMo | 8798 | 小米 MiMo | 小米账号 SSO（直连） |
| Qoder | 8797 | `gateway.qoder.com.cn` | 桌面端凭据 DPAPI 自动解密（直连） |

DSH 里填：

- 国内 `http://127.0.0.1:8787/v1`
- 国际 `http://127.0.0.1:8799/v1`（默认模型 `deepseek-v4.1-flash`）
- MiMo `http://127.0.0.1:8798/v1`（**必须带 `http://` 前缀**）
- Qoder `http://127.0.0.1:8797/v1`

> 国内 / MiMo / Qoder **直连后端**，不读系统代理 —— 切换网络、关掉代理软件都能用。
> 只有国际 `www.workbuddy.ai` 需要代理 / VPN 开启。

## 凭证

- 控制台里单选「<凭证目录A> / <凭证目录B> / <凭证目录C>」，或点「换凭证并重启」；偏好写入 `state.json`。
  （三个目录都由 `credDirs()` 提供并在网页上列出；`state.json` 里的 `intl_cred` 记录上次选择。）
- Qoder 与 MiMo **零配置**：分别从桌面端登录态自动取用，无需手工放文件。
- 目录里没有可用 `.info` 文件时，启动国际代理会明确报错，而不是静默失败。

## 数据放在哪（重要）

数据根目录（`state.json`、`hub.log`、各凭证目录、`converter-*.log`）**不再固定等于
exe 所在目录的上一级**，而是按以下优先级解析（见 `hub-go/paths.go`）：

1. 环境变量 `WORKBUDDY_HUB_ROOT`（显式指定）
2. exe 所在目录的上一级
3. exe 所在目录
4. 当前工作目录

并用「标志性文件」（`state.json` / 凭证目录 / `converter-*.log`）确认候选目录确实是数据根。

因此：**把 exe 挪到 `bin\` 或任意位置，数据仍会自动找到仓库根**，不会静默写错目录。
需要固定到别处时，设 `WORKBUDDY_HUB_ROOT` 即可。

## 目录结构

```
hub-go\
├── bin\workbuddy-hub.exe     # 构建产物（不入库）
├── scripts\
│   ├── build.bat             # 编译 → bin\
│   └── 启动控制台-Go.bat      # 启动（自动找 bin\ 或 hub-go\ 下的 exe）
├── assets\                   # 图标
├── docs\                     # 基线、交接文档、任务书、重构方案
└── *.go                      # 源码（单 main 包，见下）
```

源码按**前缀分组**（Go 要求同一包文件同目录，故不能放子目录）：

| 前缀 | 内容 |
|------|------|
| `main.go` | 入口：flag、端口检测、路由注册 |
| `hub.go` / `hub_api.go` | 控制台编排 与 HTTP API |
| `platform_windows.go` | 端口探测/清扫、进程查杀、打开浏览器 |
| `paths.go` | 数据根解析 |
| `proxy.go` / `proxy_stream.go` / `proxy_util.go` | 代理核心、流聚合、工具函数 |
| `proxy_cn.go` / `proxy_intl.go` / `proxy_mimo.go` | 三个代理的配置 |
| `proxy_qoder.go` | Qoder 凭据与 COSY 签名 |
| `qoder_proxy.go` / `qoder_thinking.go` / `qoder_stream.go` | Qoder 服务、思考控制、流改写 |
| `qoder_models.go` | Qoder 模型目录 |
| `auth.go` / `auth_mimo.go` | 凭证读取与刷新 |
| `sqlite_min.go` | 读取桌面端 SQLite（cookie 等） |
| `webui.go` | 控制台网页（内嵌进二进制） |
| `desensitize.go` | 日志脱敏 |

## 日志

| 文件 | 内容 |
|------|------|
| `hub.log` | 控制台启停、退出 |
| `converter-cn.log` | 国内代理一行摘要 |
| `converter-intl.log` | 国际代理一行摘要 |
| `converter-mimo.log` | MiMo 代理一行摘要 |
| `converter-qoder.log` | Qoder 代理一行摘要 |

默认只记一行请求/响应摘要。

启动参数（当前仅有两个）：

| 参数 | 说明 |
|------|------|
| `-port` | 控制台端口，默认 `8801` |
| `-no-browser` | 启动时不自动打开浏览器 |

> `--debug`（记录完整请求/响应体）目前**仅在源码注释中提及，尚未实现**。
> 需要排查问题时请直接看 `converter-*.log` 的摘要行，或临时在源码里加日志。

## 开发

```powershell
cd hub-go
$env:GOCACHE='E:\software\go-cache'; $env:GOPATH='E:\software\go-path'
& 'E:\software\go\bin\go.exe' vet ./...
& 'E:\software\go\bin\go.exe' test ./...
```

- 当前状态：`go vet` 通过；`go test` 全部通过（含 4 项数据根解析测试）。
- 部分测试（`*_live_test.go`、`*_e2e_live_test.go`）打真实后端，离线时会失败，
  用 `-short` 或 `-run` 挑选。
- 重构基线与历史问题记录在 `hub-go\docs\BASELINE.md`，
  重构方案见 `hub-go\docs\REFACTOR-PLAN.md`。

## 备注

- 关浏览器窗口 ≠ 退出控制台；代理继续跑。要退出请点网页上的「退出控制台」。
- 重复双击 exe 不会把正在跑的代理打掉：第二个实例发现端口占用时只打开浏览器。
- `qoder-sandbox\` 是独立的实验目录，与 Go 代理无关，未纳入版本控制。
