# zcode-adapter

把 **ZCode 官方 CLI(app-server 协议)** 桥接为本地 OpenAI 兼容接口,供 DSH 接入。

```
DSH ──HTTP──> 127.0.0.1:8795/v1/chat/completions
                  │ adapter.mjs(零依赖,Node ≥ 20)
                  ▼
          node zcode.cjs app-server --stdio   ← 官方运行时,认证/签名全部由它完成
```

## 启动

```powershell
node D:\C\Desktop\hub-go\zcode-adapter\adapter.mjs
```

可选环境变量:

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_ADAPTER_PORT` | 8795 | 监听端口 |
| `ZCODE_HARNESS` | `E:\software\Zcode\resources\glm\zcode.cjs` | 官方 CLI 路径 |
| `ZCODE_BUILTIN_CONFIG` | `E:\software\Zcode\resources\config\provider\zcode-builtin.json` | 官方内置 provider 配置 |
| `ZCODE_WORKSPACE` | `zcode-adapter\workspace` | 会话工作目录 |
| `ZCODE_PROVIDER_ID` | `account:zai-individual-coding-plan` | 模型目录为空时的回退供应商 |
| `ZCODE_ADAPTER_KEY` | 空 | 设置后要求 `Authorization: Bearer <key>` |
| `ZCODE_ADAPTER_DEBUG` | 0 | `=1` 打印协议事件 |

## DSH 接入

- **baseURL**: `http://127.0.0.1:8795/v1`
- **API Key**: 任意(或设了 `ZCODE_ADAPTER_KEY` 后填该值)
- **模型名**: `glm-5.3` / `glm-5.3-flash` / `glm-5.2` / `glm-5-turbo`

请求/响应均为标准 OpenAI Chat Completion 格式;`stream:true/false` 都支持
(流式为聚合单帧,非逐 token)。

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health` | 运行状态(harness 是否存活) |
| GET | `/v1/models` | 模型清单 |
| POST | `/v1/chat/completions` | OpenAI 兼容对话 |

## 故障处理

| 现象 | 原因与处理 |
|---|---|
| `1113 Insufficient balance` | 见下「结论与选项」——当前订阅计划只能由桌面端消费;直连路径需要账户有余额/资源包或 Individual Coding Plan |
| `Select a model before continuing` | app-server 模型目录未物化(start-plan 中继边界),适配器已自动回退显式 setModel |
| `harness exited` | 查 `ZCODE_HARNESS` 路径;适配器会在下次请求时自动重启子进程 |
| 响应 504 | 回合超时(默认 300s,`ZCODE_REQUEST_TIMEOUT_MS` 可调) |

## 结论与选项(重要)

调研结论(详见 RESEARCH.md):**本账户的 Start Plan 订阅额度被 Z.ai 设计为仅桌面端可消费**——
中继网关要求桌面端内嵌验证码 SDK 产生的风控头,官方 CLI/协议的独立使用路径
(individual coding plan / API 余额计费)对本账户返回 1113。适配器管道本身已全链路打通
(官方运行时 + 官方签名 + 真实网关调用),要让 DSH 真正拿到模型回复,三选一:

1. **Z.ai 充值余额或购买资源包**(通用 API 计费)→ 本适配器立即可用,零改动;
2. 购买 **Individual Coding Plan** 并运行一次官方 `node zcode.cjs login`(浏览器点击授权)
   → 同上;
3. Start Plan 的工作继续在 Zcode 桌面端内进行(现状)。

不修改 ZCode 安装文件;不绕过任何验证机制(验证码头生成属桌面端 UI 能力,未模拟)。
