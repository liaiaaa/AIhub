# 交接：cc-switch 接管 Codex 聚合 + hub-go UI 重构

- 日期：2026-10-13
- 当前状态：**三个阶段全部完成**。① 原版 cc-switch 聚合验证通过；② fork 定制版 **Hub Switch** 已构建安装并验证通过（原版 cc-switch 已卸载）；③ hub-go UI 已重构为侧边栏分页（仪表盘/服务与凭证/聚合与网关），聚合管理以 Hub Switch 为准、用量统计随 Hub Switch 自带而撤下。详见 [验证报告](./2026-10-13-cc-switch-phase1-verification.md)。
- **唯一剩余项**：`:8790` 退役（默认不自启 → UI 折叠 → 观察后删代码）。建议观察 Hub Switch 稳定运行数日后再动。
- 结论：hub-go 不并入 cc-switch；cc-switch 作为 Codex 的控制面/聚合面，hub-go 继续作为专属上游网关。验证通过后逐步退役 hub-go 的 `:8790`。
- 源码位置：
  - hub-go：`/home/<user>/opt/AI_test/hub-go`
  - cc-switch 源码：`/home/<user>/opt/AI_test/cc-switch-main`
- cc-switch：v4.0.10，MIT License。若 fork/改名，保留原 LICENSE 和版权声明。

## 当前运行态快照

- hub-go 进程：`/home/<user>/opt/AI_test/hub-go/hub-go/bin/aihub -no-browser`
  - PID 当时为 `54868`
- 已监听端口：
  - `:8801` 控制台
  - `:8787` 国内 WorkBuddy
  - `:8797` Qoder
  - `:8799` 国际 WorkBuddy
  - `:8790` Codex 聚合口
- 当前 Codex 配置仍走老方案：
  - provider：`aihub`
  - base_url：`http://127.0.0.1:8790/v1`
  - wire_api：`responses`
  - top-level model：`dom.glm-5.3-flash`
  - profile `aihub` 默认模型：`intl.deepseek-v4.1-flash`
- Codex 配置备份：
  - 原位备份：`~/.codex/config.toml.bak-agg`
  - 本次交接快照：`/home/<user>/opt/AI_test/hub-go/docs/codex-config.toml.bak-20261013-aihub-8790`
- 聚合白名单快照：`/home/<user>/opt/AI_test/hub-go/agg_models.json`

当前聚合模型：

| 线路 | 聚合 slug | 实际模型 |
|---|---|---|
| intl | `intl.deepseek-v4.1-flash` | `deepseek-v4.1-flash` |
| dom | `dom.glm-5.3-flash` | `glm-5.3-flash` |
| dom | `dom.deepseek-v4-flash` | `deepseek-v4-flash` |
| qoder | `qoder.Qwen3.8-Flash` | `Qwen3.8-Flash` |
| intl | `intl.hy4-preview` | `hy4-preview` |

## 目标架构

```text
Codex
  ↓
cc-switch 本地路由 / Codex 聚合
  ↓ 按模型分发
hub-go 各线路端口
  ↓
上游服务
```

hub-go 继续保留并负责：

| 线路 | 本地端点 |
|---|---|
| 国内 WorkBuddy | `http://127.0.0.1:8787/v1` |
| 国际 WorkBuddy | `http://127.0.0.1:8799/v1` |
| Qoder | `http://127.0.0.1:8797/v1` |
| Trae | `http://127.0.0.1:8795/v1` |
| Zcode | `http://127.0.0.1:8796/v1` |
| MiMo | `http://127.0.0.1:8798/v1` |

cc-switch 负责：

- Codex 供应商配置管理；
- 多供应商模型映射；
- Codex 聚合；
- 本地路由 / 协议转换；
- 通用用量统计。

## 第一阶段：原版 cc-switch 验证，不要先 fork

目的：先把迁移链路跑通，避免同时引入 fork 改动导致问题难定位。

### 1. 添加 Codex 自定义供应商

在 cc-switch 的 Codex 页面，为 hub-go 每条线路添加一个自定义供应商：

| cc-switch 供应商名 | 端点地址 | 上游格式 |
|---|---|---|
| HubGo Dom | `http://127.0.0.1:8787/v1` | Chat Completions |
| HubGo Intl | `http://127.0.0.1:8799/v1` | Chat Completions |
| HubGo Qoder | `http://127.0.0.1:8797/v1` | Chat Completions |
| HubGo Trae | `http://127.0.0.1:8795/v1` | Chat Completions |
| HubGo Zcode | `http://127.0.0.1:8796/v1` | Chat Completions |

API Key 填占位值即可，例如 `hub-local`。hub-go 目前不按该 Key 鉴权。

### 2. 手动配置模型映射

不要依赖 models.dev 自动补全这些私有模型。每个供应商手动维护模型映射：

建议迁移映射：

| 供应商 | 菜单显示名 | 实际请求模型 | 上下文窗口 |
|---|---|---|---:|
| HubGo Intl | DeepSeek 4.1 Flash Intl | `deepseek-v4.1-flash` | `1000000` |
| HubGo Intl | HY4 Preview Intl | `hy4-preview` | 待确认 |
| HubGo Dom | GLM 5.3 Flash Dom | `glm-5.3-flash` | 待确认 |
| HubGo Dom | DeepSeek V4 Flash Dom | `deepseek-v4-flash` | 待确认 |
| HubGo Qoder | Qwen3.8 Flash | `Qwen3.8-Flash` | 待确认 |

注意：迁移后不要继续使用 `intl.` / `dom.` / `qoder.` 前缀 slug。线路归属改由 cc-switch 的供应商决定，模型名应是 hub-go 对应线路接受的真实模型名。

### 3. 打开 Codex 聚合

1. cc-switch 侧栏选择 Codex；
2. 进入「聚合」标签；
3. 把 HubGo 系列供应商加入聚合名单；
4. 设置默认供应商；
5. 点「切换到聚合模式」；
6. 彻底重启 Codex。

Codex 只在启动时读取聚合模型目录，改模型列表后必须彻底重启；只重开窗口无效。

### 4. 验证清单

必须全部测过再进入第二阶段：

- Codex `/model` 能看到聚合模型；
- 每个模型至少完成一次非流式请求；
- 流式输出正常；
- 工具调用正常；
- 思考等级正常，不出现 400；
- 长上下文行为正常；
- cc-switch 用量统计有数据；
- hub-go 日志确认请求进入对应线路；
- 停掉某条 hub-go 线路后，只有对应模型失败，其他模型不受影响；
- cc-switch 退出/重启后行为符合预期；
- 老 `:8790` 方案仍可作为回退。

## 第二阶段：fork / 定制专属 cc-switch

验证通过后再 fork。

### 推荐改动范围

1. 改名与图标
   - 例如显示名：`Hub Switch` / `Li Switch`；
   - 修改 Tauri identifier、应用图标；
   - 保留 MIT LICENSE 和原版权声明。

2. 内置 HubGo 预设
   - 为 Codex 增加一组私有预设；
   - 预设直接指向 hub-go 本地端口；
   - 预填真实模型映射、上下文窗口、思考能力。

3. 精简界面
   - 隐藏不相关的供应商预设、赞助商内容、无关工具；
   - 不建议一次性大删功能，先做可见性裁剪。

4. 增加 HubGo 健康面板
   - 后续可在 cc-switch 里调用 hub-go `/api/state`；
   - 展示线路运行状态、额度摘要、签到状态、风控状态；
   - 这是增强项，优先级低于迁移验证。

5. 不移植 hub-go
   - 不把 Go 代理逻辑重写进 Rust；
   - 继续保持 hub-go 独立进程。

## `:8790` 退役策略

不要立即删除代码。

建议顺序：

1. 迁移验证期间保留 `:8790`；
2. Codex 切到 cc-switch 聚合并稳定运行后，停止使用 `:8790`；
3. hub-go 中把 `:8790` 默认不自启；
4. UI 中折叠或隐藏聚合配置；
5. 观察若干天后确认不再回退，再删除聚合相关代码。

相关代码入口：

- 聚合实现：`/home/<user>/opt/AI_test/hub-go/hub-go/proxy_agg.go`
- 聚合端口常量：`AggPort = 8790`
- 自动启动：`aggAutostart()`
- 白名单文件：`/home/<user>/opt/AI_test/hub-go/agg_models.json`
- Web UI 聚合块：`/home/<user>/opt/AI_test/hub-go/hub-go/webui.go`

## hub-go UI 重构目标

采用侧边栏分页方案，不做单页手风琴。

页面拆分：

1. 仪表盘
   - 服务状态总览；
   - 快捷签到；
   - 今日 Token 使用摘要；
   - 聚合/网关健康提示。

2. 服务与凭证
   - WorkBuddy / Qoder / Trae / Zcode / MiMo 账号列表；
   - 每个服务卡片默认只显示当前生效凭证和状态摘要；
   - 多凭证放到 Drawer 或展开列表；
   - 风控凭证用浅红背景或红点；
   - 已停止线路降低透明度或支持「仅显示已启动」。

3. 聚合配置
   - 第一阶段暂时保留旧 `:8790` 配置用于回退；
   - 后续重点改为展示 cc-switch 聚合状态或 HubGo 预设说明；
   - 退役后可删除白名单编辑、上下文上限等 UI。

4. 统计报表
   - Token 明细从首页移出；
   - 「按天」和「按模型」用 Segmented 切换；
   - 数字列使用等宽字体或 `font-variant-numeric: tabular-nums`。

## 回退方案

如果 cc-switch 聚合验证失败：

1. cc-switch 将 Codex 回到直连或关闭接管；
2. 恢复 Codex 配置：

```bash
cp /home/<user>/opt/AI_test/hub-go/docs/codex-config.toml.bak-20261013-aihub-8790 ~/.codex/config.toml
```

3. 启动 hub-go；
4. 确认 `:8790` 已监听；
5. 彻底重启 Codex；
6. 用 `intl.deepseek-v4.1-flash` 或 `dom.glm-5.3-flash` 验证。

## 给 DSH 的执行顺序

1. 先读本文件；
2. 不要改 hub-go 代码；
3. 先在原版 cc-switch 中配置 HubGo Codex 供应商；
4. 配模型映射；
5. 开启 Codex 聚合；
6. 彻底重启 Codex；
7. 跑完验证清单；
8. 把验证结果和失败请求日志记录到本目录；
9. 通过后再考虑 fork cc-switch；
10. 最后再处理 hub-go `:8790` 退役和 UI 重构。
