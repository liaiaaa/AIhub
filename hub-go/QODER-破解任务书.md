# Qoder CN 反代破解 · 任务书（交给执行 AI 的完整提示词）

> **用法**：把本文档整个交给一个有本机 shell / 文件权限的 AI，让它照着干。
> **目标一句话**：破解 Qoder CN 推理接口的鉴权，实现纯 HTTP 直连反代，最终并入 workbuddy-hub。
> **纪律**：按 §5 路线顺序执行，速度优先——先抄近路（本地数据库 / 一次抓包 + 已知算法反推），**不要从零啃打包二进制**。单条路卡 30 分钟就换下一条。

---

## 0. 任务与验收标准

背景：用户有一个 Go 代理聚合器 workbuddy-hub（源码 `D:\C\Desktop\workbuddy-hub\hub-go`，控制台 8801，现有代理端口 8787 国内 WorkBuddy / 8798 MiMo / 8799 国际 WorkBuddy）。目标是新增一条 **Qoder CN（阿里 Qoder 国内版）纯 HTTP 直连**线路：反向代理对外暴露 OpenAI 兼容接口，对内用逆向出的鉴权直调 Qoder 网关推理接口，流式转回。

**完成 = 以下三项交付全部达成：**

1. **实测请求配方**——对 `POST https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation` 的完整调用方法：URL、全部 headers（含 COSY 头明文 schema）、body 字段表、SSE 事件解析格式。证据 = curl/脚本真实调用返回模型输出。
2. **反代原型**——Go 单文件（可先用 Node PoC），监听 **8797**（空闲端口），对外 `POST /v1/chat/completions`（stream）+ `GET /v1/models`，对内翻译成 Qoder 推理请求，SSE 流式翻译回 OpenAI chunk 格式。
3. **交接文档** `QODER-HTTP-交接文档.md`——凭据获取/续期（`dt-` token、machineToken、COSY 签名怎么算）、有效期、错误码含义、复现步骤。

速度指标：**半天内拿到 curl 实证，一天内原型跑通**；反代首 token < 5s，签名开销 < 100ms。

---

## 1. 资产坐标（2026-09-22 实测确认，全部有效）

| 资产 | 路径 | 说明 |
|---|---|---|
| **Qoder CN 桌面端（Electron，主逆向目标）** | 安装于 `C:\Users\lihua\AppData\Local\Qoder CN\`（`Qoder CN Launcher\` 版本目录树里找 `resources\app.asar`） | JS 签名器在此。asar 是归档：`npx asar e app.asar app-extract` 解包后 grep。**安装目录只读，拷出来分析** |
| 桌面端用户数据 | `C:\Users\lihua\AppData\Roaming\com.qodercn.app.stable\` | **`auth.v1.dat`**（422B，`dt-` 设备令牌直接从这里读，此前已成功提取）；`auth.machine-id`（37B）；**`chat-session-turn-payload-buffer.sqlite`（3.5MB，可能直接存了聊天请求完整 body！）**；`main.sqlite`（5.8MB）；`qoder-data.v1.json` |
| CLI 数据目录 | `C:\Users\lihua\.qoder-cn\` | `.auth\user`（1240B，CLI 加密凭据 blob）、`.auth\machine_id`、`.bin\umid-win32-x64-48d1294f147c9d89\runtime-info.exe`（machineToken 生成器） |
| **CLI 网络日志（金矿）** | `C:\Users\lihua\.qoder-cn\logs\runs\*\qodercli.log` | 完整请求日志：operation 名、method、完整 URL、traceId、requestSetId、超时契约。grep `agent_chat_generation\|gateway.qoder.com.cn` 共 **1.1 万+ 命中**，可完整还原 API 调用编排（见 §3） |
| 会话转录 | `C:\Users\lihua\.qoder-cn\projects\`、`logs\sessions\` | 过往 agent 会话 jsonl（含此前逆向实验记录，可查但别精读浪费时间） |
| **CLI 本体** | 已删除 | 原为 `~/.qoder-cn/bin/qoderclicn/qoderclicn-1.1.59.exe`（Bun 打包，222MB）。如动态 hook 确实需要，可经官方渠道重装，**用完删掉**（用户不想留 CLI） |
| 沙盒工作目录 | `D:\C\Desktop\workbuddy-hub\qoder-sandbox\` | 所有实验代码、解包产物、抓包记录放这里 |
| 环境 | Node `E:\software\nodejs\node.exe`；Go `E:\software\go\bin\go.exe`（GOCACHE=`E:\software\go-cache`，GOPATH=`E:\software\go-path`） | |
| machineToken（本机） | `P1gAuQTUR1HKGk0h8gjRW3heL7u8jHBmj4zp_ElDzDXm-Go0ngjtOr4LAyxhktdGncmW8uAT0PyOT664xF-Vnp0y` | runtime-info.exe 产物；如过期用它重新生成。仅本机使用 |

⚠️ **别被误导**：`E:\software\db\Doubao` 是字节的豆包桌面端（liblark.dll / VolcEngineRTC 那套），**与 Qoder 无关**，不要去逆它。

---

## 2. 已逆向出的 COSY 鉴权协议（直接复用，不要重新推）

请求头：

```
Authorization: Bearer COSY.<payload_b64>.<sig>
```

- **`<payload_b64>`** = payload（结构体/JSON）用**自定义 base64 字母表**编码。不是加密、不隐藏语义——**可以反向解码**。标准表 → 自定义表：

  ```
  标准: A B C D E F G H I J K L M N O P Q R S T U V W X Y Z a b c d e f g h i j k l m n o p q r s t u v w x y z 0 1 2 3 4 5 6 7 8 9 + /
  自定: _ d o R T g H Z B K c G V j l v p C , @ a F S x # D P u N J m e & i * M z L O E n ) s U r t h b f % Y ^ w . ( k I Q y X q W A !
  ```
  （映射方向、padding 以桌面端 JS 为准；字母表含 `# , & % ^ . ( ) ! @ * _` 等字符，进 header/URL 时注意转义。）

- **`<sig>`** = `MD5(payload_b64 + "\n" + cosy_key + "\n" + cosy_date + "\n" + body + "\n" + path)` 的 hex（五段 `\n` 连接）。`cosy_key`、`cosy_date` 是 payload/请求中的字段，语义在 JS 里核对。⚠️ body 参与签名：**改 body 必须重签**。

- **COSY 秘钥常量**：`d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==`（即 `"war, war never changes"` 的标准 base64）。

- **CLI 本地凭据加密**（`.auth\user` 的格式）：AES-256-GCM，key = `scryptSync("qoder-cli-cn-credentials", salt, 32)`，代码内标记串 `"qoder-cli-cn"`（`Qh` 变量）。

**速度关键**：COSY payload 只是换字母表 base64，**抓到任意一个真实请求头，反解 `<payload_b64>` 就得到 payload 明文 schema**。所以先抓包/读本地库，不是先读代码。

---

## 3. API 编排（从日志 1.1 万条命中还原，按此顺序复现）

| # | 调用 | 用途 |
|---|---|---|
| 1 | `GET /api/v3\|v5/service/region/endpoints` | 端点选举（返回 center/inference/security-inference endpoint，本区全是 `https://gateway.qoder.com.cn`） |
| 2 | `GET /api/v2/config/getDataPolicy` | 数据策略 |
| 3 | `GET /api/v2/model/list` | 模型目录（拿 model id，抄给反代 `/v1/models`） |
| 4 | **`POST /algo/api/v2/service/pro/sse/agent_chat_generation`** | **推理主接口**（operation=`sendRemoteChatAsk`，SSE，requestClass=`infer-sse`） |
| 5 | `POST /algo/api/v2/service/business/finish` | 业务完成/计费回报（同 traceId，可省） |
| 6 | `POST /api/v1/tracking` | 埋点（可省） |

- 账号类接口用裸 `Bearer dt-...` 实测 200；**推理接口用裸 `dt-` 会 500，必须走 COSY 头**。
- CLI 走 httpdns 并把网关域名钉到 IP（`47.104.160.223` / `139.129.37.231`）；反代直连域名即可，网络不稳再参考钉 IP。
- 日志里 `requestSetId=...`、`traceId=...` 是请求配对线索；推理前可能有 session/quest 创建类调用，抓包时留意 `agent_chat_generation` **之前的**请求。

---

## 4. 死路清单（每条都烧过几小时，禁止重走）

1. ❌ **盲猜爆破 `.auth\user` / CLI 凭据解密**：AES-GCM/CBC/CFB × 9+ 种 scrypt KDF 参数、DPAPI 4 种调法、protobuf 解析、machine_id XOR——全部失败。如确需解密，唯一正确姿势是**动态 hook**（Bun/Node 运行时里 hook `scryptSync`/`createDecipheriv` 打印入参），不要盲猜。
2. ❌ 拿裸 `dt-` token 当 Bearer 调推理接口 → 500（账号接口可以，推理不行）。
3. ❌ 子进程包一层 qoderclicn CLI 转发：已废弃。Windows 32K 命令行上限、Bun 二进制每次弹控制台窗、大上下文 300s 超时。目标是纯 HTTP 直连，别回头。
4. ❌ 在 222MB Bun 打包二进制里静态硬啃字符串：慢且 CLI 已删。桌面端 app.asar 是同一套代码且可读。

---

## 5. 作战路线（按序执行；单条卡 30 分钟就换下一条）

### Step 0 ｜先翻本地数据库（30 分钟，零网络）
- 打开 `C:\Users\lihua\AppData\Roaming\com.qodercn.app.stable\chat-session-turn-payload-buffer.sqlite` 和 `main.sqlite`（拷到沙盒再查）。找聊天轮次的**请求 payload 缓冲**——很可能直接存着 `agent_chat_generation` 的完整 body（甚至 headers）。有就直接拿到 schema，跳到 Step 2。
- 顺手 strings/JSON 读 `auth.v1.dat`（确认 `dt-` 字段结构）和 `qoder-data.v1.json`。

### Step 1 ｜抓一个真实推理请求（30 分钟）
- 启动 Qoder CN 桌面端（加 `--remote-debugging-port=9222`，Electron 通用），CDP 的 `Network`/`Fetch` 域抓 `POST /algo/api/v2/service/pro/sse/agent_chat_generation`；或 mitmproxy（CA 导入系统信任）。
- 只发一句测试 prompt（如 "1+1=? 只回答数字"），省额度。
- 拿到三样东西：①完整 headers（尤其 `Authorization: COSY.x.y`）；②**原始 body 字节**（若是 protobuf，用 CDP 拿 base64 原文）；③它前面的配对请求（session/quest 创建之类）。
- 立刻按 §2 字母表反解 `COSY.<payload_b64>` → payload 明文 schema。

### Step 2 ｜静态核对语义（1–2 小时）
- 解包 `app.asar`，grep 字母表常量 `doRTgHZBKcGVjlvpC` / `COSY` / `cosy_key` / `agent_chat_generation`，找到 payload 构造函数与签名函数。
- 对照 Step 0/1 的样本，确定每个字段语义：哪些必填、哪些是时间戳/nonce（新鲜度窗口？防重放？）、cosy_key/cosy_date 怎么来的、machineToken 和 `dt-` 进不进 payload。

### Step 3 ｜复刻签名器 + 重放（1 小时）
- 50 行 Go/Node 复刻：构造 payload → 自定义 base64 → MD5 链签名 → **原样重放**抓到的请求，验证 200 + SSE。
- 再做逐字段减法测试（去掉某字段/换 body 看服务端反应），确定最小必需字段集。

### Step 4 ｜token 生命周期（30 分钟）
- `dt-` 设备令牌：从 `auth.v1.dat` 读；桌面端刷新它的调用是哪个（账号接口）；有效期多长。写进文档。
- machineToken：`runtime-info.exe` 重新生成方法；判断是否参与鉴权。
- 若推理需要前置 session/quest 创建，把这步编排进反代（Step 1 会暴露）。

### Step 5 ｜反代原型（2–3 小时）
- Go 单文件，风格照抄 `D:\C\Desktop\workbuddy-hub\hub-go\proxy_mimo.go`（AuthProvider + SSE 直通），监听 8797。
- 对外：`POST /v1/chat/completions`（stream）+ `GET /v1/models`；对内：翻译成 `agent_chat_generation` body，SSE 事件逐帧翻译回 OpenAI chunk。
- 凭据从文件/环境变量读，不硬编码。首 token < 5s。

### Step 6 ｜交接文档 + 汇报
- 写 `QODER-HTTP-交接文档.md` 到沙盒目录：配方、字段表、token 生命周期、错误码、复现命令。
- 汇报格式：curl 一行实证 + 反代启动命令 + 已知风险（token 过期表现、风控触发条件）。

---

## 6. 硬约束（违反 = 返工）

1. **不许修改 `D:\C\Desktop\workbuddy-hub\hub-go\` 下任何文件**，也不许杀它的进程（hub 正在运行：8801 控制台 + 8787/8798/8799 代理）。所有实验在 `D:\C\Desktop\workbuddy-hub\qoder-sandbox\` 做；最后交付 Go 单文件 + 文档，由用户决定何时合入 hub。
2. **不许破坏 Qoder CN 桌面端**：不改安装目录（拷出来分析）、不退账号、不动 `auth.v1.dat` 原件。桌面端是用户日常干活的工具。
3. 凭据（machineToken、`dt-`、账号 token）只在本机用：不上传、不写进交付代码（从文件/环境变量读）。
4. 这是用户**自己的账号**，目的是把自己的 AI 流量聚合进自己的 hub（个人使用）；不做任何对外分发、共享、售卖。
5. 测试请求用最小 prompt，不烧额度；不在用户真实工作会话里动手脚。
6. Windows 踩坑提示：PowerShell here-string 里 `$变量` 解析容易出幺蛾子 → 脚本写成文件用 `E:\software\nodejs\node.exe` 跑；受限沙盒里 Node `child_process` 捕获 stdio 的 spawn 会 EPERM（用 `stdio:'inherit'` 或 PowerShell 自带管道）；命令行 32K 上限。

---

## 7. 最终交付清单（打勾交卷）

- [ ] curl 一行：真实返回 Qoder 模型输出（贴输出片段作证据）
- [ ] COSY payload 字段表 + 签名算法说明（与实测一致）
- [ ] `qoder_proxy.go`（或 Node PoC + Go 成品）：8797 端口，OpenAI 兼容进出
- [ ] `QODER-HTTP-交接文档.md`
- [ ] 风险说明：token 过期/续期、可能的风控点、失败时的回退方案
