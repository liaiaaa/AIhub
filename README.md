# AIHub

把 WorkBuddy 系列的五个后端（国内 / 国际 / MiMo / Qoder / Zcode）统一转成本地 **OpenAI 兼容**
接口的 Go 单文件程序。桌面控制台一键启停，代理全部跑在同一个进程里。

```
浏览器控制台  http://127.0.0.1:8801
      │
      └─ aihub.exe（单进程）
           ├─ 国内  127.0.0.1:8787 → codebuddy 国内
           ├─ 国际  127.0.0.1:8799 → www.workbuddy.ai
           ├─ MiMo  127.0.0.1:8798 → 小米 SSO
           ├─ Qoder 127.0.0.1:8797 → gateway.qoder.com.cn
           └─ Zcode 127.0.0.1:8796 → api.z.ai（GLM 系列）
```

## 快速开始

**最省事**：双击桌面上的 **`AIHub`** 快捷方式 —— 它会先重新编译，再启动控制台。

| 步骤 | 操作 |
|------|------|
| 一键（推荐） | 双击桌面 `AIHub` 快捷方式 = 编译 + 启动 |
| 只编译 | 双击 `hub-go\scripts\build.bat` → 产物在 `hub-go\bin\aihub.exe` |
| 只启动 | 双击 `hub-go\scripts\启动控制台-Go.bat`（不重编译，秒开） |
| 控制 | 网页上手动点「启动 / 停止」；底部可「全部停止 / 退出」 |

编译需要用 `E:\software\go\bin\go.exe`（缓存也放 E 盘，不占 C 盘）。

> 启动默认**全部停止**：程序会先清扫 8787/8796/8797/8798/8799 的残留监听，
> 再由你手动选择开启哪一个。
>
> 重复启动不会把正在跑的代理打掉：第二个实例发现 8801 被占用时，
> 只打开浏览器就退出。

## 在 Linux 上跑

前置依赖：Go ≥ 1.27（`/usr/local/go`），以及 `libsecret-tools`（提供 `secret-tool`，
Qoder 主密钥要从 GNOME 密钥环取；`sudo apt install -y libsecret-tools`）。
Go 安装包用 `https://mirrors.aliyun.com/golang/go1.27.2.linux-amd64.tar.gz`，
npmmirror **不镜像 Go 二进制**（`mirrors/go/` 那个路径 404）。

```bash
./hub-go/scripts/build.sh     # 编译 → hub-go/bin/aihub（不带 -H=windowsgui）
./hub-go/scripts/start.sh     # 后台启动控制台；前台跑用 hub-go/bin/aihub -no-browser
```

**桌面快捷方式**（对应 Windows 上「双击 AIHub = 编译 + 启动」）：

```bash
./hub-go/scripts/install-desktop-entry.sh              # 装到 桌面 + Activities 应用菜单
./hub-go/scripts/install-desktop-entry.sh --autostart  # 额外加「登录后自动启动」
```

双击「AIHub 控制台」会一次做完：有 go 就重新编译 → 起控制台（已在跑就复用）→
自动拉起 `dom intl qoder trae` 四条线路 → 开浏览器 → 弹一条通知汇总结果
（例：`控制台 :8801 · dom=✓ intl=✓ qoder=✓ trae=✓`）。可调项：
`AIHUB_LINES`（改自动启动的线路）、`AIHUB_NO_BUILD=1`（跳过编译）、
`AIHUB_NO_BROWSER=1`（不开浏览器）。脚本是 `scripts/aihub-launch.sh`，
快捷方式由 `scripts/install-desktop-entry.sh` 按仓库当前位置生成，挪目录后重跑一次即可。

`hubRoot` 解析规则不变（`AIHUB_ROOT` > 从 exe 逐级向上找 `state.json`/凭证目录 > 工作目录），
所以把整个仓库目录拷到 Linux 上，`state.json`、`<凭证目录A>*`、`wbatrest.key` 会原样被找到。

各线路在 Linux 上的凭据来源：

| 线路 | 端口 | Linux 上怎么拿凭据 | 状态 |
|------|------|--------------------|------|
| Qoder | 8797 | 本机桌面端 `~/.config/com.qodercn.app.stable/auth.v1.dat`；主密钥走 Electron safeStorage：GNOME 密钥环条目 `application="Qoder CN App"` → PBKDF2-SHA1(`saltysalt`,1,16) → **v11** 头 AES-128-CBC。已实测跑通（`hub.log` 里会打「qoder 凭据解密方式：keyring:Qoder CN App+pbkdf2-16+aes-cbc」） | ✅ 零配置 |
| Zcode | 8796 | 桌面端凭据的信封密钥绑定 `{platform}:{homedir}:{username}`，从 Windows 拷来的文件在 Linux 解不开 → 走手动 Key | ✅ 需一次手工 |
| 国际 | 8799 | 凭证目录（`<凭证目录A>` / `<凭证目录B>` / `<凭证目录C>`）本来就在数据根，直接可用；出网靠**系统代理**（Linux 读 GNOME dconf，本机实测 `mode=manual` → `127.0.0.1:10808`）。2026-10-09 实测：凭据加载 + 后端余额查询均通过 | ✅ 已通 |
| 国内 | 8787 | 从 Windows 拷 `workbuddy-desktop.info`（信封解密是纯 Go，`wbatrest.key` 已在数据根）。查找顺序：`WORKBUDDY_AUTH_DIR` → `<数据根>/CodeBuddyExtension/Data/Public/auth/` → 数据根直接放文件 → **数据根下任意子目录里的同名文件**（所以 `国内workbuddy凭证/` 这种自起名字的目录能直接被找到）。2026-10-09 实测：凭据解密 + 后端余额查询（2,677 / 5,200 积分）通过 | ✅ 已通 |
| Trae | 8795 | 拷 Windows 的 `%APPDATA%\Trae CN\User\globalStorage\storage.json`（`tc` 信封是纯算法、跨平台可解，只要这一个文件）。放 `<数据根>/Trae CN/User/globalStorage/storage.json`，或用 `TRAE_USER_DATA_DIR` 指定。2026-10-09 实测：解出登录态 + 余额查询（1,000 / 1,000 积分）通过 | ✅ 已通 |
| MiMo | 8798 | 依赖 Windows 桌面端的 Xiaomi Cookie 库 | ❌ 不支持 |

Zcode 手动 Key 三选一：控制台粘贴、`ZCODE_API_KEY` 环境变量、或数据根放 `zcode_api_key.txt`。

Zcode 的**免费额度中继**（`tools\zcode-relay`）在 Linux 上不会自拉起（`zcode_relay_spawn_other.go`
是空实现），需要手动启动中继进程后设 `ZCODE_RELAY_URL`；不设也不会静默回落，
直连是充值余额计费通道。

Linux 上可用的环境变量：

| 变量 | 用途 |
|------|------|
| `AIHUB_ROOT` | 固定数据根目录 |
| `QODER_APP_DATA` | 覆盖 Qoder 桌面端数据目录 |
| `QODER_SAFE_STORAGE_PASSWORD` | safeStorage 主口令（自动查不到时手工指定） |
| `QODER_SAFE_STORAGE_KEY` | 已派生密钥（base64/hex，16 或 32 字节） |
| `QODER_SAFE_STORAGE_APP` | 密钥环条目 `application` 属性取值 |
| `QODER_RUNTIME_INFO_EXE` | 覆盖设备信息工具路径（默认找 `~/.qoder-cn/.bin/runtime-info-*`） |
| `QODER_MACHINE_TOKEN` | runtime-info 取不到时的 machineToken 兜底 |
| `WORKBUDDY_AUTH_DIR` | 国内线路放 `workbuddy-desktop.info` 的目录 |
| `TRAE_USER_DATA_DIR` | Trae 的 `User` 目录（内含 `globalStorage/storage.json`） |

> 用 `scripts/start.sh` 起服务时，上面这些加代理类变量会被 `--setenv` 带进 systemd 用户服务
> ——systemd 服务**不继承当前 shell 的环境**，直接在终端 `export` 对服务无效。

系统代理：环境变量 `HTTP_PROXY/HTTPS_PROXY/NO_PROXY` 优先；没有环境变量时回退读系统代理设置——
Windows 是注册表 Internet Settings，Linux 是 GNOME dconf（`org.gnome.system.proxy`，
`mode=manual` 时取 `http`/`https` 子模式的 host:port）。这一步是必需的：v2rayN 之类
客户端只改系统代理、不导环境变量，本机实测 shell 和 systemd 用户环境里都没有代理变量。
`mode=auto`（PAC）不解，按直连处理；非 GNOME 桌面（KDE/i3）请自己设环境变量。
取值进程内只读一次，**改完代理要重启控制台**。

## 五个代理

| 代理 | 端口 | 后端 | 认证方式 |
|------|------|------|----------|
| 国内 | 8787 | codebuddy 国内 | 凭证文件（直连） |
| 国际 | 8799 | `www.workbuddy.ai` | 凭证目录（读系统代理，需 VPN） |
| MiMo | 8798 | 小米 MiMo | 小米账号 SSO（直连） |
| Qoder | 8797 | `gateway.qoder.com.cn` | 桌面端凭据 DPAPI 自动解密（直连） |
| Zcode | 8796 | `api.z.ai`（GLM 系列） | 桌面端凭据自动解密 / 手动 API Key（直连） |

DSH 里填：

- 国内 `http://127.0.0.1:8787/v1`
- 国际 `http://127.0.0.1:8799/v1`（默认模型 `deepseek-v4.1-flash`）
- MiMo `http://127.0.0.1:8798/v1`（**必须带 `http://` 前缀**）
- Qoder `http://127.0.0.1:8797/v1`
- Zcode `http://127.0.0.1:8796/v1`（默认模型 `glm-5.3`）

> 国内 / MiMo / Qoder / Zcode **直连后端**，不读系统代理 —— 切换网络、关掉代理软件都能用。
> 只有国际 `www.workbuddy.ai` 需要代理 / VPN 开启。

> **Zcode 计费说明**：本代理走 Z.ai 官方 API Key 直连（充值余额 / 资源包计费）。
> **免费额度通道（2026-09-27 已通）**：运行 `tools\zcode-relay\start-zcode-relay.bat`
> 启动本地 start-plan 中继（内置验证码自动求解，凭据自动取自桌面端），Zcode 代理
> 自动探测接入，Start Plan / 活动赠送额度即可经代理消费；中继失败不回落计费直连。
> 直连路径无余额时上游返回 1113，代理会给出明确指引。
> 2026-09-27 已对齐官方客户端指纹并接入控制面（参照 TriDefender/zcode-api 逆向）：
> 请求携带桌面端身份头（UA/平台/OS/设备号，两套 header 组），模型目录从
> `client/configs` 实时刷新（快照兜底），`agent/configs` 端点改写表 fail-open
> 跟随；套餐余额经 `billing/balance` 透出到 `/health` 的 `quota` 字段。

## 思考档位（reasoning effort）

**hub-go 侧规则**（2026-10-08 用户要求：客户端没发档位就用最高档）：

- 客户端**完全没表达思考意图**时，各线路按 `ThinkingDefault` 注入档位；显式
  `reasoning_effort` / `thinking` 对象 / `enable_thinking` / budget / 消息里的
  `[no-thinking]` 标记一律尊重，不覆盖。
- 国内 / 国际：注入 `reasoning_effort=max`（`proxy_cn.go`、`proxy_intl.go`）。
- Qoder / Zcode：取该模型目录里的**最高档**（`qoder_thinking.go`、`zcodeHighestEffort`），
  不是目录默认档。
- MiMo / Trae：`ThinkingDefault` 留空 = 纯透传（上游档位语义未验证）。

**DSH 里能不能选，取决于 DSH 自己的 profile，不取决于 hub-go 的 `/v1/models`。**
DSH 的模型目录来自 `~\.dsh\profiles\desktop\cordis.patch.yml` 里
`llm-pi-ai → providers.<线路>.models[].reasoningEfforts`：**没写这个键就没有思考等级下拉**
（键=下拉里的档位，值=实际发给本机的 `reasoning_effort`）。2026-10-08 已给
workbuddy / workbuddyai / zcode 的模型补上，实测档位表现：

| 线路 · 模型 | 已声明档位（实测 HTTP 200 且思考长度随档位变化） | 备注 |
|---|---|---|
| 国内 · deepseek-v4-flash | minimal / low / medium / high / xhigh / max | `off` → 400 `11150 模型不支持该思考强度`，故不提供 |
| 国内 · glm-5.3-flash | minimal / low / medium / high / max | max 最深（一道难题上 5.8K 字符思考），`low` 有时直接不出思考链 |
| 国内 · hy3-preview-agent | low / medium / high / max | 各档差异很小（230~435 字符），`off` 被接受但同样不关掉思考 |
| 国际 · deepseek-v4.1-flash | minimal / low / medium / high / xhigh / max | `off` 同样被拒 |
| Zcode · GLM-5.3 / GLM-5.3-Flash | low / high / max | 与 `zcode_models.go` 目录一致 |
| MiMo | 未声明 | 当前无额度，未验证，保持无下拉 |

选「Provider default」= 不发档位 = 由 hub-go 注入最高档。

两条改 profile 时的坑：

- **改完必须重启 DSH 宿主**才生效。已在本机 asar 里查过：没有 `reloadProfile` /
  `profile-reload` / `configVersion` 之类的热加载入口，profile 只在启动时读一次
  （唯一的 fs watcher 是 dev 环境的 bundle HMR，与配置无关）。
- 一个模型条目里**不能出现两个 `reasoningEfforts:`**。DSH 的 schema 会拒重复键，
  PyYAML 却静默后者覆盖前者，所以校验时要用带重复键检测的 loader
  （`qoder-sandbox/yamlcheck.py`）。合法档位名固定为
  `off / minimal / low / medium / high / xhigh / max`（DSH 里的 `THINKING_LEVELS`）。

## 凭证

- 国内线路也支持多凭证（2026-10-13）：数据根下含 `workbuddy-desktop.info`
  的一级子目录都会被认作国内凭证目录（内置「国内workbuddy凭证 / CodeBuddy凭证」）。
  「CodeBuddy凭证」来自 CodeBuddy IDE 的登录态（state.vscdb safeStorage 解密导出，
  主密钥在 GNOME 密钥环 application="CodeBuddy CN"），是独立账号独立积分池；
  服务页下拉单选 + 「换凭证并重启」，签到会对**全部**国内凭证各签一次。
- 国际线路控制台里单选「<凭证目录A> / <凭证目录B> / <凭证目录C> / <凭证目录D>」，或点「换凭证并重启」；偏好写入 `state.json`。
  （三个目录都由 `credDirs()` 提供并在网页上列出；`state.json` 里的 `intl_cred` 记录上次选择。）
- Qoder 与 MiMo **零配置**：分别从桌面端登录态自动取用，无需手工放文件。
- Zcode 同样零配置：自动解密 `%USERPROFILE%\.zcode\v2\credentials.json`（AES-256-GCM
  信封）取 Coding Plan API Key；也可用环境变量 `ZCODE_API_KEY` 或控制台手动粘贴。
- 目录里没有可用 `.info` 文件时，启动国际代理会明确报错，而不是静默失败。
- **国内 WorkBuddy 5.7.6 起凭据字段被加密**：`workbuddy-desktop.info` 里的
  `account.nickname` / `auth.accessToken` / `auth.refreshToken` 变成
  `{"$wbEncrypted":1,"envelope":"<base64>"}`（AES-256-GCM），hub-go 会**就地解密**后再用，
  仍然共用桌面端那份实时凭据（桌面端刷新 token 后 hub-go 立刻跟着拿到新的）。
  - 解密密钥缓存在数据根的 `wbatrest.key`（32 字节密钥的 base64，已 gitignore）。
    密钥不落磁盘、只存在于桌面端主进程里，因此它是一次性从运行中的 WorkBuddy 取的
    （`--inspect` + `electron.workbuddyStorage.loggerGet()`，过程见
    `hub-go/workbuddy_atrest.go` 顶部注释）。
  - 缺文件 / 密钥被新版本轮换时，控制台会报中文提示（不会再把整块状态页打崩）；
    重新取一次密钥即可。刷新回写时会重新加密，不会把桌面端的文件降级成明文。

## 数据放在哪（重要）

数据根目录（`state.json`、`hub.log`、各凭证目录、`converter-*.log`）**不再固定等于
exe 所在目录的上一级**，而是按以下优先级解析（见 `hub-go/paths.go`）：

1. 环境变量 `AIHUB_ROOT`（显式指定，最高优先级）
2. 从 exe 所在目录**逐级向上**查找（最多 4 级），命中即用
3. 当前工作目录（兜底，例如 `go run`）

判断某目录「是不是数据根」只看两个**权威标志**：`state.json` 或凭证目录
（`<凭证目录A>` / `<凭证目录B>` / `<凭证目录C>`）。

> **刻意不把 `*.log` 当标志。** 日志是 hub 自己写出来的，一旦某个目录被误写了日志，
> 它下次就会被误判成数据根，错误会自我固化、再也回不到真正的根。
> （这是实际踩过的坑：exe 在 `bin\` 时，`hub-go\` 因残留日志被误判为数据根，
> 日志一直写在 `hub-go\` 而不是仓库根。）

因此：**把 exe 放在 `bin\` 或任意位置，数据仍会自动找到仓库根**，不会静默写错目录。
需要固定到别处时，设 `WORKBUDDY_HUB_ROOT` 即可。

## 目录结构

```
hub-go\
├── bin\aihub.exe     # 构建产物（不入库）
├── scripts\
│   ├── build.bat             # 只编译 → bin\
│   ├── 编译并启动.bat         # 编译 + 启动（桌面快捷方式指向它）
│   └── 启动控制台-Go.bat      # 只启动（自动找 bin\ 或 hub-go\ 下的 exe）
├── assets\                   # 图标
├── docs\                     # 基线、开发说明、交接文档、重构方案
└── *.go                      # 源码（单 main 包，见下）
```

> ⚠️ `scripts\*.bat` **必须保持 ASCII 编码 + CRLF 换行**。
> 中文 Windows 的 cmd.exe 按 GBK 读取 .bat，UTF-8 中文注释会导致解析错乱、
> 脚本直接跑不起来（这个也实际踩过）。改脚本时请沿用现有风格。

源码按**前缀分组**（Go 要求同一包文件同目录，故不能放子目录）：

| 前缀 | 内容 |
|------|------|
| `main.go` | 入口：flag、端口检测、路由注册 |
| `hub.go` / `hub_api.go` | 控制台编排 与 HTTP API |
| `platform.go` | 端口探测/清扫、控制台日志（跨平台） |
| `platform_windows.go` / `platform_other.go` | 占用者查询、进程查杀、打开浏览器（`netstat`+`taskkill` / `/proc`+`xdg-open`） |
| `proxy_qoder_oskey_windows.go` / `_other.go` | Qoder 桌面端凭据解密（DPAPI+GCM / Electron safeStorage） |
| `exec_hidden_*.go`、`zcode_relay_spawn_*.go` | 子进程隐藏窗口 / 中继自拉起，同为 `_windows`+`_other` 成对 |
| `paths.go` | 数据根解析 |
| `proxy.go` / `proxy_stream.go` / `proxy_util.go` | 代理核心、流聚合、工具函数 |
| `proxy_cn.go` / `proxy_intl.go` / `proxy_mimo.go` | 三个代理的配置 |
| `proxy_qoder.go` | Qoder 凭据与 COSY 签名 |
| `qoder_proxy.go` / `qoder_thinking.go` / `qoder_stream.go` | Qoder 服务、思考控制、流改写 |
| `qoder_models.go` | Qoder 模型目录 |
| `qoder_envelope.go` | Qoder 请求信封（对齐官方客户端 wire 形态，思考开关所在） |
| `proxy_zcode.go` / `zcode_*.go` | Zcode（Z.ai）代理：服务 / 目录 / 思考映射 / 流改写 |
| `auth_zcode.go` | Zcode 桌面端凭据解密（enc:v1 AES-256-GCM 信封） |
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
| `converter-zcode.log` | Zcode 代理一行摘要 |

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

Linux：

```bash
cd hub-go
go vet ./... && go build ./...          # 交叉检查 Windows 侧是否编译得动：
GOOS=windows go build -o /dev/null .    # 需 -ldflags "-s -w -H=windowsgui" 才是产物形态
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
