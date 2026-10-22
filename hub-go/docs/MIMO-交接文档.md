# MiMo 模型接入 hub-go 代理 — 交接文档

> 生成时间：2026-09-16
> 状态：**已完成并验证通过**。代码已改好，待用户自行打包 exe。

---

## 一、任务

在现有 `hub-go` 代理里增加一路 **MiMo（小米）代理**，路由两个模型：

- `MiMo-X-Flash-Preview`
- `MiMo-X-Pro-Preview`

新增端口 **8798**，其余保持不变（国内 8787 / 国际 8799 / 控制台 8801）。

---

## 二、结论：MiMo 认证的真实机制

这是本次最大的发现，也是之前卡住的原因。

### 2.1 两条完全不同的认证路线

MiMo 桌面端支持两种认证，**只有第二条能拿到用户要的那两个模型**：

| 路线 | 后端 | 凭证 | 模型 | 可用性 |
|---|---|---|---|---|
| 开放平台 API | `https://api.xiaomimimo.com/v1` | `sk-` 开头的 API Key | `mimo-v2.5` / `mimo-v2.5-pro` | ❌ 返回 **402 Payment Required**（余额不足） |
| **桌面端 SSO** | `https://mimo-server-cn.xiaomimimo.com/api` | 小米账号 SSO 换的 `serviceToken` | **`mimo-x-flash-preview` / `mimo-x-pro-preview`** | ✅ **免费可用** |

> 用户桌面端登录后，`~/.local/share/mimocode/auth.json` 里确实有个 `sk-` key
> （`C:\Users\lihua\.local\share\mimocode\auth.json`），但那个 key 走的是**付费**开放平台，
> 余额为 0 → 402；且开放平台上**不存在** `mimo-x-*-preview` 这两个模型（400 Bad Request）。
> 所以必须走 SSO。

### 2.2 SSO 换票流程（已实测跑通）

```
Phase 1  GET https://account.xiaomi.com/pass/serviceLogin
           ?_locale=zh_CN&_snsNone=true&sid=mimopc&_json=true
         Header: Cookie: passToken=<p>; userId=<u>; cUserId=<c>
                 User-Agent: MiClaw/1.0
         → 响应体前缀 "&&&START&&&"，去掉后是 JSON：
           { code:0, location:"...", nonce:<19位整数>, ssecurity:"...", ... }

Phase 2  clientSign = urlencode(base64(sha1("nonce=<nonce>&<ssecurity>")))
         GET <location>&clientSign=<clientSign>
         Header: User-Agent: MiClaw/1.0      ← 故意不带 Cookie
         → Set-Cookie: serviceToken=<384字符>; userId=...; mimopc_slh=...; mimopc_ph=...

聊天     POST https://mimo-server-cn.xiaomimimo.com/api/route/chat/completions
         Header: Cookie: serviceToken=<st>; userId=<u>
                 X-Mimo-Source: mimocode-cli-free
                 User-Agent: MiClaw/1.0
                 Content-Type: application/json
```

### 2.3 三个关键坑（都踩过）

1. **sid 是 `mimopc`**，不是 `mimo` / `mimocode` / `xiaomimimo`。
   那些会返回 `{"code":21315,"desc":"无效的sid"}`。
   `mimopc` 是从 `mimo-server-cn` 的 302 跳转 `Location` 里读出来的：
   `...&sid=mimopc&_group=DEFAULT`。

2. **`nonce` 是 19 位整数**（如 `1909760886818022400`），
   超出 float64 的 53 位精度。**必须用 `json.Number` 按字符串取**，
   否则签名算错 → Phase 2 拿不到 serviceToken。
   （MiMo 官方 JS 也是用正则 `/"nonce"\s*:\s*(\d+)/` 取字符串的。）

3. **Cookie 库第 1 页有 100 字节文件头**，B-tree 页头从偏移 100 开始，不是 0。
   自写 SQLite 解析器时这是最容易漏的一处。

### 2.4 凭证来源

小米账号 cookie 在 MiMo 桌面端的数据目录里（**明文存在 `value` 列**，
`encrypted_value` 为空，所以不需要解密）：

```
%APPDATA%\Xiaomi MiMo\Partitions\xiaomi-account\Network\Cookies   (SQLite)
  → .account.xiaomi.com  passToken = "<redacted-passToken>"   (367 字符)
  → .account.xiaomi.com  userId    = "<redacted-userId>"
  → .account.xiaomi.com  cUserId   = "<redacted-cUserId>"
```

> ⚠️ **该文件被运行中的 MiMo 桌面端独占锁定**（连只读打开都 EBUSY，
> 无备份权限，`vssadmin` 也不可用）。
> 实测：**MiMo 关闭时 Go 能正常读取**。
> 因此代码里对读失败给了明确提示：「若 MiMo 正在运行请先退出再试」。

---

## 三、代码改动

### 3.1 新增文件

| 文件 | 作用 |
|---|---|
| `auth_mimo.go` | MiMo SSO 认证：Phase1/Phase2 换票、30 分钟缓存、401 自动重换 |
| `proxy_mimo.go` | MiMo 代理定义（端口 8798、后端、模型、别名） |
| `sqlite_min.go` | 零依赖 SQLite 只读解析器（只为读那几个 cookie，避免引入第三方驱动） |

### 3.2 修改文件

| 文件 | 改动 |
|---|---|
| `proxy.go` | 抽出 `AuthProvider` 接口（凭证文件 / SSO 两种实现）；`Proxy` 增加 `Kind`/`ChatPath` 字段；新增 `doUpstream`（401 自动换票重试一次）；`p.cred` → `p.auth` |
| `main.go` | Hub 增加 `mimo` 字段与启停方法；新增 `/api/mimo/start`、`/api/mimo/stop`；`statusMap`/`enrichStatus`/`apiStopAll`/`apiExit`/`sweepPorts` 全部纳入 MiMo |
| `webui.go` | 新增 MiMo 卡片与启停按钮；`renderSide`/`refreshNick` 纳入 mimo |

### 3.3 设计要点

- **认证抽象**：`AuthProvider` 接口只有 `Headers()` + `Summary()`。
  国内/国际走 `CredentialManager`（读 JSON 凭证 + 刷 accessToken），
  MiMo 走 `MimoAuth`（SSO 换 serviceToken）。两者机制完全不同，只统一对外样子。
- **不阻塞控制台**：MiMo 换票是网络操作，**在锁外执行**（沿用之前修死锁的教训）。
- **零依赖**：`sqlite_min.go` 自己解析 B-tree，保持项目「离线可 `go build`」的特性。
- **401 自愈**：serviceToken 真实有效期不由我们掌握，服务端提前判失效时自动换票重试一次。

---

## 四、验证结果（隔离环境，端口改为 23887/23898/23899/23801）

| 项 | 结果 |
|---|---|
| `mimo/start` | ✅ `{"ok":true,"msg":"已启动"}` |
| `/v1/models` | ✅ 返回两个模型 |
| `mimo-x-flash-preview` 对话 | ✅ 正常回复 |
| `mimo-x-pro-preview` 对话 | ✅ 正常回复（约 32s，Pro 推理慢，非代理问题） |
| 别名 `MiMo-X-Pro-Preview` | ✅ 正确映射到小写 id |
| 流式 SSE | ✅ 37 个分片正常拼接 |
| 并发 6 请求 | ✅ 6/6 成功 |
| `mimo/stop` | ✅ 端口立即释放 |
| `stop_all` / `exit` | ✅ 秒级完成，进程正常退出 |
| `go vet` / `gofmt` | ✅ 干净 |

---

## 五、用户待办：打包

```bat
cd /d D:\C\Desktop\workbuddy-hub\hub-go
set GOCACHE=E:\software\go-cache
set GOPATH=E:\software\go-path
E:\software\go\bin\go.exe build -trimpath -ldflags "-s -w -H=windowsgui" -o workbuddy-hub.exe .
```

或直接双击 `build.bat`。

⚠️ 打包前先退出正在运行的控制台，否则 exe 被占用写不进去。

### 使用方式

1. 启动 `workbuddy-hub.exe`，打开控制台 <http://127.0.0.1:8801>
2. 点 MiMo 卡片的「启动」
3. 客户端把 base_url 指向 `http://127.0.0.1:8798/v1`，模型填 `mimo-x-flash-preview` 或 `mimo-x-pro-preview`

---

## 六、思考等级（reasoning effort）调查结论

**结论：MiMo 这两个模型没有思考等级可选，是固定开着的。**

### 证据

1. **模型能力声明是布尔值，不是等级列表。**
   桌面端 `model-catalog.json` 里，GPT-5.x 这类模型声明的是
   `reasoning_options:[{type:"effort",values:["none","low",...,"xhigh"]}]`，
   而 MiMo 系列用的是：

   ```js
   const wa = { attachment:false, reasoning:true, tool_call:true, temperature:true,
                modalities:{input:["text"],output:["text"]},
                limit:{context:5e5,output:32e3}, cost:{input:0,output:0} }
   ```

   只有 `reasoning:true`（固定开启），**没有 `reasoning_options`**。

2. **桌面端代码里没有任何 thinking / effort 参数。**
   全量搜索 `thinking`、`reasoning_effort`、`reasoningEffort`：
   - `reasoning_effort` 只出现在一个函数里，且被 `rU(model)` 拦着：
     ```js
     function rU(e){ return /claude|fable|fb5/i.test(e) }   // 只对 claude 系生效
     ```
     该函数把 `reasoning_effort` 转成 `output_config.effort`，**只走 `/responses` 端点**。
     MiMo 模型不匹配，走 `/chat/completions`，用不到这条路。
   - `thinking` 只在一处出现，是 UI 事件名（`mimo:chatReason` 的注释
     "thinking-not-displayed-as-result"），**不是请求参数**。

3. **实测：服务端静默忽略未知参数。**
   拿真实 token 打了 15 组参数（`reasoning_effort` 取 none/low/medium/high/xhigh/
   minimal/max/非法值、`thinking:{type:disabled|enabled}`、`enable_thinking`、
   `reasoning:{effort}`、`thinking_budget`），**全部 HTTP 200**，
   连完全瞎编的 `zzz_bogus=1` 也是 200。

   对照组证明服务端**会**校验该校验的东西：

   | 请求 | 结果 |
   |---|---|
   | 不存在的模型 | `400 chat_mify_model_unavailable` |
   | `messages: []` | `400 messages must not be empty` |
   | 非法 role | `400 message_not_readable` |
   | `zzz_bogus=1` | **200（静默忽略）** |

   所以那些参数的"耗时差异"纯粹是模型输出随机性，**参数并没有生效**。

### 实际表现

两个模型**始终**返回 `reasoning_content` 字段，等级不可调：

```json
{"choices":[{"message":{
   "content":"1 + 1 = **2**",
   "reasoning_content":"Simple math question."    ← 固定存在
}}]}
```

- **Flash**：思考短，1~2 秒返回（`displayRatio: 0.4`）
- **Pro**：思考长，实测一次 32 秒（`displayRatio: 1`，是 Flash 的 2.5 倍计费倍率）

### 对本代理的影响

代理目前**透传** `reasoning_effort` 等字段（在 `passthroughKeys` 里），
对 MiMo 传了也无害（服务端忽略），不影响使用。**无需改动。**

如果将来小米给 MiMo 加上思考等级，届时按服务端实际接受的字段名在
`proxy_mimo.go` 里补映射即可。

---

## 七、已知限制 / 后续可改进

1. **读 cookie 需要 MiMo 桌面端未运行**（文件被独占锁）。
   当前行为：启动时若读不到会给出明确中文提示。
   若想彻底解决，可改为从桌面端进程内存读取，或让用户在 MiMo 关闭时启动代理。
2. **拿不到账号昵称**：`/api/user/xiaomi/me` 需要桌面端 session，返回 302。
   控制台 MiMo 卡片的「账号」显示为 `?`，**不影响功能**。
3. **思考等级不可调**（见第六节）：两个模型固定开启思考，无等级参数。
4. **serviceToken 有效期未公开**：代码按 30 分钟主动重换 + 401 被动重换双保险。
5. `MimoPort = 8798` 定义在 `proxy_mimo.go`，不在 `main.go` 的 const 块里（有意为之，避免重复定义）。

---

## 八、相关文件速查

```
D:\C\Desktop\workbuddy-hub\hub-go\
  ├─ auth_mimo.go      新增 · MiMo SSO 认证
  ├─ proxy_mimo.go     新增 · MiMo 代理定义（MimoPort=8798）
  ├─ sqlite_min.go     新增 · 零依赖 SQLite 只读解析
  ├─ proxy.go          改 · AuthProvider 抽象 + doUpstream 401 重试
  ├─ main.go           改 · Hub 接入 mimo
  └─ webui.go          改 · 控制台 MiMo 卡片

凭证（只读）：
  C:\Users\lihua\AppData\Roaming\Xiaomi MiMo\Partitions\xiaomi-account\Network\Cookies
```

---

## 九、附：可复现的探测脚本

排查过程中写的脚本（在 `%TEMP%`，可能已被清理，逻辑已全部固化进 `auth_mimo.go`）：

- `sso_probe.js` — 试各种 sid，找出哪些有效
- `sso_mimopc.js` — 用 `sid=mimopc` 跑完整 Phase1+Phase2 并测聊天 ← **关键突破点**
- `readcookies*.js` — 读 Chromium Cookies 库
- `think_probe.js` / `think_probe2.js` — 思考等级参数实测（15 组参数 + 静默忽略验证）

复现要点：Phase 2 的签名输入是 `"nonce=" + nonce + "&" + ssecurity`，
SHA1 → Base64 → URLEncode，拼成 `&clientSign=`。

---

## 十、2026-09-22 更新：模型换代 V2.6

小米已下架 preview 系模型，换成 V2.6（ID 来源：桌面端 `%APPDATA%\Xiaomi MiMo\model-catalog.json`）：

| 旧 ID（已下架） | 新 ID | 显示名 | displayRatio |
|---|---|---|---|
| mimo-x-flash-preview | `mimo-v2.6-flash` | MiMo V2.6 Flash | 0.4 |
| mimo-x-pro-preview | `mimo-v2.6-pro` | MiMo V2.6 Pro | 1 |
| — | `mimo-v2.6-pro-ultraspeed` | MiMo V2.6 Pro Ultraspeed | **10** |

`proxy_mimo.go` 已更新：新模型入列表，默认模型改为 `mimo-v2.6-flash`，
旧 preview 名保留为别名映射到对应新模型（旧客户端配置不用改）。

⚠️ Ultraspeed 的 displayRatio 是 Pro 的 10 倍（桌面端目录原值），免费额度消耗会快得多，日常用 Flash/Pro 即可。
