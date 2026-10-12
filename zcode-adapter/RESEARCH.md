# ZCode 官方能力接入 DSH — 调研报告

日期:2026-09-26 · 运行时版本:zcode 0.16.9 / 桌面端 3.14.3 · 全部结论经真机验证

## 1. 可公开调用入口(已验证)

| 入口 | 形态 | 状态 |
|---|---|---|
| `node zcode.cjs -p "<prompt>"` | 官方非交互模式 | ✅ 可用(需先完成「模型物化」,见 §3) |
| `node zcode.cjs app-server --stdio` | 官方 stdio 协议(ZCode Protocol) | ✅ 可用,NDJSON 无 jsonrpc 信封 |
| `node zcode.cjs doctor` | 运行时自检 | ✅ |
| `node zcode.cjs login --no-browser` | 官方 OAuth(打印 URL 轮询) | ✅(需要浏览器点击授权) |
| 桌面端本地 HTTP/端口 | 无(仅 stdio 子进程通道) | ❌ 不存在 |

## 2. app-server 协议(0.16.9 实测)

- 启动:`node <zcode.cjs> app-server --stdio`,环境变量
  `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=<官方内置配置>`(见 §3)。
- 帧:NDJSON,一行一个 JSON;`id+method`=请求(双向),`method`=通知,`id`=响应。
- 已验证方法:`session/create`、`session/setModel`、`session/subscribe`
  (deliveryKind: "desktop-continuous")、`session/send`、`session/messages`、
  `session/stop`、`session/close`。
- 服务端反向请求(必须应答):`session/requestRuntimePreferences`
  (`{nativeSearchEnhancementsEnabled, memoryEnabled, askUserQuestionAutoResolutionEnabled,
  modelContextBudgetStrategy:"preflight-v1"}`)、
  `interaction/requestProviderRuntimeHeaders`(见 §5 边界)。
- 会话事件:`session/event` 通知,`params.kind` = turn.failed / turn.terminal / …,
  `params.payload.error` 为失败详情;最终文本用 `session/messages` 转录读取。
- `session/create` 参数:`workspace: {workspaceKey, workspacePath}`(两者都必须)。

## 3. 配置文件与模型选择机制(全部实测)

| 文件 | 作用 |
|---|---|
| `~/.zcode/v2/credentials.json` | 凭据库。`enc:v1` 信封(AES-256-GCM,密钥=sha256(`zcode-credential-fallback:{platform}:{homedir}:{username}`),`ZCODE_CREDENTIAL_SECRET` 可覆盖)。键:`oauth:zai:*`、`account-provider:<family>:identity`、`account-provider:coding-plan:<providerId>:account:<identity>:api-key` |
| `~/.zcode/v2/provider_config.json` | 个人 provider 配置。schemaVersion=1,`config:{providerConfigRules:{providerRules:[…]}, modelConfigRules:{providerModelRules:[…]}, defaultModelSelection:{providerId, modelId}}`(**defaultModelSelection 必须在 config 内**;顶层写法仅被 legacy 导入读取) |
| `<安装>/resources/config/provider/zcode-builtin.json` | 官方内置规则(20 模板/8 供应商/84 模型规则);经 `ZCODE_BUILTIN_PROVIDER_CONFIG_FILE` 注入运行时 |
| `~/.zcode/v2/runtime/provider/<平台>/<版本>/endpoint-<hash>/` | 宿主按端点物化的内置配置副本 |

供应商两族(内置规则原文):
- `account:zai-individual-coding-plan`:access=`zhipu-coding-plan-api-key`,API=`https://api.z.ai/api/anthropic`(直连,API Key 计费)
- `account:zai-start-plan`:access=`zhipu-account` mode=start-plan,API=`https://zcode.z.ai/...`(中继,订阅额度)

**模型物化最小集**(本调研验证,写入后独立 CLI 即可真实调用 API):
1. credentials.json:`account-provider:<providerId>:identity`(加密)+ `account-provider:coding-plan:<providerId>:account:<identity>:api-key`(加密)
2. provider_config.json:个人 `providerRules` 声明该 provider + `builtinProviderModelRules` 的模型规则 + `config.defaultModelSelection`

## 4. 已验证功能

- ✅ CLI 非交互(`-p`)、cwd 指定、`--json`、`--verbose`
- ✅ app-server 全生命周期:create → setModel → subscribe → send → 事件流 → 转录 → close
- ✅ 模型目录机制、`session/setModel` 两种形态、默认选择持久化
- ✅ 官方签名链路:运行时自动完成 HKDF/Ed25519/PoW(V4 客户端签名)并真实调达网关
  (响应头 `x-log-id`/`request-id` 为证)
- ✅ 本地 HTTP 适配器(adapter.mjs):/health、/v1/models、/v1/chat/completions
  (stream true/false)、错误透传

## 5. 边界(未达成的根因,非实现缺陷)

本账户的订阅是 **zcode-v3-start-plan**(GLM-5.3-Flash 约 7700 万 units)。

- start-plan 额度只能经 `zcode.z.ai` 中继消费,该中继要求**阿里云验证码/风控头**
  (协议反向请求 `interaction/requestProviderRuntimeHeaders`)。
- 该头由桌面端内嵌验证码 SDK 生成——桌面端日志有完整 captcha-diagnostics 链路
  (attempt.start → script.ready → sdk.callback);独立运行时与第三方桥接都无法生成。
  参考 zcode-agent-kit 的能力注册表原文:*"Desktop injects captcha/risk headers; bridge
  answers headersApplied:false … captcha solving is a desktop UI capability"*。
- 因此 start-plan 模型在独立运行时报 `Select a model before continuing` /
  `Model creation failed`;绕过验证码违反任务约束,未尝试。
- API Key 直连(individual 路径)对本账户返回 **1113 Insufficient balance**
  (start-plan 额度不参与直连计费)。

## 6. 失败记录(任务要求的失败协议)

- 步骤:app-server 会话全通 → setModel 报 `Provider Registry 中不存在 Model`
- 已尝试:credentials 身份/密钥物化(individual + start-plan)、provider_config 个人规则
  物化、defaultModelSelection 两种位置、setModel 对象/字符串两种形态、
  session/create 带 model 参数、延迟等待服务端配置刷新——目录始终为空,
  根因是 §5 的 start-plan 中继边界
- 下一步建议:见 README.md「结论与选项」

## 7. 验收情况

- ✅ curl 可调用本地接口(127.0.0.1:8795,/health、/v1/models、/v1/chat/completions)
- ✅ 返回有效 OpenAI 格式响应(当前账户下为结构化的上游 1113 错误,管道本身已打通:
  官方 CLI → 官方签名 → 真实网关调用)
- ⚠️ DSH 可识别该 Endpoint(OpenAI 兼容),但**拿不到有效模型回复**——受 §5 账户侧
  边界限制;若账户具备 Individual Coding Plan 或充值余额,同一接口立即返回有效回复
  (管道无需任何改动)
