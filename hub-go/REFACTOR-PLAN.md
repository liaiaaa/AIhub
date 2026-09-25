# WorkBuddy Hub (Go) — 工程化重构方案

> 目标：把 `hub-go\` 从「源码 + 9 个 exe 备份 + 一堆临时脚本 + 日志」的混放状态，
> 整理成可维护的 Go 工程结构，且**不破坏当前正在运行的 workbuddy-hub.exe**。

## 一、现状诊断

### 1.1 目录污染

`hub-go\` 根目录混放了 5 类东西：

| 类别 | 文件 | 问题 |
|------|------|------|
| 源码 | 12 个 `.go` | 应分层 |
| 测试 | 8 个 `_test.go` | 与源码平铺 |
| 构建产物 | `workbuddy-hub.exe` | 与源码混放 |
| **旧 exe 备份** | 9 个 `.bak*` / `-backup` / `-hidden`（约 80MB） | **纯垃圾** |
| 临时脚本 | `_find_go.ps1` `_review_*.ps1/.txt` | 一次性调试残留 |
| 文档 | 2 个交接/任务书 `.md` | 应归 `docs\` |

父目录同样混乱：4 个 `.log`（最大的 1.1MB）、`state.json`、`hub-go.ico`、
一个失效的 `启动控制台.bat`（指向已不存在的 `D:\C\Desktop\workbuddy-hub`）。

### 1.2 代码问题

**A. 单文件过大**
- `proxy_qoder.go` 1642 行 / 51KB — 一个文件扛了认证 + 模型目录 + 思维链 + 流改写
- `proxy.go` 884 行、`main.go` 643 行

**B. 致命的设计耦合 — `hubRoot`**

```go
var hubRoot = func() string {
    exe, _ := os.Executable()
    return filepath.Dir(filepath.Dir(exe))  // exe 的上级的上级
}()
```

exe 在 `hub-go\` → `hubRoot` = `D:\C\Desktop\hub-go\`。
凭证目录、`state.json`、所有日志的定位**全部依赖 exe 的物理位置**。
一旦挪动 exe 或换构建输出目录，静默失效（找不到凭证 → 国际代理起不来）。

**C. 硬编码过期路径（现存 Bug）**

`proxy_intl_test.go:17`：
```go
hubRoot = `D:\C\Desktop\workbuddy-hub`   // 该目录已不存在
```
→ `TestCredDiscovery` 失败。这是**当前 `go test` 唯一的失败项**。

**D. 职责未分层**
`main.go` 里 HTTP 路由、进程管理、端口清扫、Windows API、文件日志全混在一起。

## 二、已验证的基线（重构前）

```
go vet ./...   → 通过（EXIT=0）
go test ./...  → 1 个失败 / 34 个用例
                 FAIL: TestCredDiscovery（硬编码过期路径）
```

**重构的验收标准就是：这两个数字不能变差，且 TestCredDiscovery 转为通过。**

## 三、目标结构

```
hub-go\
├── bin\workbuddy-hub.exe        # 构建产物（已 gitignore）
├── cmd\hub\main.go              # 入口：flag / 路由 / 启动
├── internal\
│   ├── hub\        hub.go       # 编排：启停、状态、HTTP API
│   ├── proxy\      proxy.go     # 代理核心 + 流聚合
│   │               proxy_intl.go  proxy_cn.go  proxy_mimo.go
│   ├── qoder\      qoder.go     # Qoder（原 proxy_qoder.go 拆分）
│   │               models.go  thinking.go  stream.go
│   ├── auth\       auth.go      # 凭证读取 + 刷新
│   │               auth_mimo.go
│   ├── platform\   winapi.go    # hideConsoleWindow / 进程 / 端口
│   │               paths.go     # hubRoot 解析（改为可配置）
│   └── sqlite\     sqlite_min.go
├── web\            index.go     # 控制台页面（embed）
├── docs\           *.md         # 交接文档、任务书
├── scripts\        build.bat  启动控制台-Go.bat
├── archive\                     # 归档（不删除）
├── go.mod
└── README.md
```

## 四、分阶段实施

### 阶段 0：建立安全网（必须先做）
1. `git init` + 首次提交 → 任何一步出错都能 `git diff` 回看、`git checkout` 回退
2. 记录基线：`go vet` / `go test` 输出存 `docs\BASELINE.md`
3. 归档垃圾：9 个旧 exe、`_*.ps1`、`.bak` 源码 → `archive\`
4. 父目录日志、临时产物归位

**阶段 0 结束时目录已明显变干净，且代码零改动。**

### 阶段 1：修掉硬编码路径（小、独立、立刻见效）
- `proxy_intl_test.go` 改用相对/环境变量定位，消除过期绝对路径
- `TestCredDiscovery` 转绿

### 阶段 2：解耦 `hubRoot`
- 抽出 `internal/platform/paths.go`
- 解析顺序：exe 上级 → 再上级 → 环境变量 `WORKBUDDY_HUB_ROOT` 覆盖
- **保持当前 exe 位置不变时行为完全一致**（按用户要求不动正在用的 exe）

### 阶段 3：分层迁移
- 按包拆分，逐个包迁移 + 每次迁移后跑 `go test`
- 先迁移低耦合的（sqlite / desensitize / platform），再 auth，最后 proxy / qoder

### 阶段 4：拆分巨型文件
- `proxy_qoder.go` 1642 行 → auth / models / thinking / stream 四个文件
- 纯搬运，不改逻辑，`git diff` 保证只有移动

### 阶段 5：文档与脚本
- README 重写（删除已失效的 Python 版说明）
- `scripts\build.bat` 输出到 `bin\`
- `.gitignore`

## 五、风险控制

| 风险 | 对策 |
|------|------|
| 正在运行的 exe 被破坏 | **不动 exe 位置**；所有改动只在源码层 |
| 重构引入行为变化 | 每阶段后 `go test`；阶段 4 纯文件搬运不改逻辑 |
| 包循环依赖 | 自底向上迁移：platform → sqlite → auth → proxy → qoder → hub |
| 无法回退 | 阶段 0 先 git init + 提交 |

## 六、明确不做

- 不移动 `workbuddy-hub.exe`（用户要求保留原位以便后续修 bug）
- 不删除任何东西 —— 统一移到 `archive\`
- 不改代理的协议/转发逻辑（本阶段只做结构工程化）
