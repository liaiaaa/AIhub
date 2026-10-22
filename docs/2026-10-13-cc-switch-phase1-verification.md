# 第一阶段验证报告：cc-switch 接管 Codex 聚合

- 日期：2026-10-13
- 结论：**聚合链路验证通过**（12 项验证清单 10 项全绿、1 项环境阻塞、1 项待 Key）；老 `:8790` 回退方案保持可用。未改动 hub-go 任何代码。
- 执行环境说明：本次由中断的 dsh 会话交接后继续完成。中断点前的证据（11:39–11:47 的完整运行）与本报告中的重跑证据（12:10–12:17）合并采信。

## 一、当前架构状态

```text
Codex（CLI / 桌面版）
  ↓ ~/.codex/config.toml：model_provider=custom → http://127.0.0.1:15721/v1
cc-switch 本地路由 :15721（聚合 Stack 模式）
  ↓ 按模型名分发：默认供应商无前缀，其余 ccs-<key>/<model>
hub-go 各线路（:8787 Dom / :8799 Intl / :8797 Qoder / :8795 Trae / :8796 Zcode）
  ↓
上游（copilot.tencent.com / www.workbuddy.ai / Qoder / Trae / Z.ai）
```

- cc-switch 供应商（cc-switch.db，`app_type='codex'`，`meta.apiFormat=openai_chat`，上游按 Chat Completions 转换）：

| 供应商 id | 名称 | 端点 | 模型 |
|---|---|---|---|
| hubgo-intl | HubGo Intl | `http://127.0.0.1:8799/v1` | deepseek-v4.1-flash、hy4-preview |
| hubgo-dom | HubGo Dom | `http://127.0.0.1:8787/v1` | glm-5.3-flash、deepseek-v4-flash |
| hubgo-qoder | HubGo Qoder | `http://127.0.0.1:8797/v1` | Qwen3.8-Flash |
| hubgo-trae | HubGo Trae | `http://127.0.0.1:8795/v1` | glm-5.2 |
| hubgo-zcode | HubGo Zcode | `http://127.0.0.1:8796/v1` | glm-5.3、glm-5.3-flash |

- 聚合 Stack（`~/.cc-switch/live-state.json`）：默认供应商 `hubgo-intl`，成员 5 家，key 映射 intl/dom/qoder/trae/zcode。
- Codex 侧模型目录（`~/.codex/cc-switch-model-catalog.json`）：8 个模型全部 `visibility=list`，会出现在 `/model`。
- 2026-10-13 12:35 显示名调整（用户要求「模型名（国内）/（国际）」风格，脚本 `.dsh-work/step3_rename_display.py`，改前有 DB 备份）：

| slug | 调整前 | 调整后 |
|---|---|---|
| deepseek-v4.1-flash | DeepSeek 4.1 Flash Intl | **DeepSeek V4.1 Flash（国际）** |
| hy4-preview | HY4 Preview Intl | **HY4 Preview（国际）** |
| ccs-dom/glm-5.3-flash | GLM 5.3 Flash Dom（HubGo Dom） | **GLM 5.3 Flash（国内）** |
| ccs-dom/deepseek-v4-flash | DeepSeek V4 Flash Dom（HubGo Dom） | **DeepSeek V4 Flash（国内）** |
| ccs-qoder/Qwen3.8-Flash | Qwen3.8 Flash（HubGo Qoder） | 不变 |
| ccs-trae/glm-5.2 | GLM 5.2 Trae（HubGo Trae） | 不变 |
| ccs-zcode/glm-5.3 | GLM 5.3 Zcode（HubGo Zcode） | 不变 |
| ccs-zcode/glm-5.3-flash | GLM 5.3 Flash Zcode（HubGo Zcode） | 不变 |

  原理（源码确认）：默认供应商的模型展示用 `displayName` 原样；其余 Stack 成员拼 `"<displayName>（<供应商名>）"`（`mode/stack.rs:341` + `codex_config.rs:2519`）。故 Intl 的 `displayName` 直接带（国际），Dom 的供应商名改为「国内」并去掉显示名里的 Dom 后缀。改后冒烟测试路由正常。

## 二、验证清单结果（12 项）

| # | 验证项 | 结果 | 证据 |
|---|---|---|---|
| 1 | Codex `/model` 能看到聚合模型 | ✅ 通过 | `codex debug models` 输出 8 个模型，全部 visibility=list |
| 2 | 每个模型至少一次非流式请求 | ✅ 通过* | Dom/Qoder/Trae 见本次重跑；Intl 见 11:39 运行；*Zcode 阻塞见下 |
| 3 | 流式输出正常 | ✅ 通过* | 各线 26–339 个 SSE 事件，首事件 0.9–4.6s |
| 4 | 工具调用正常 | ✅ 通过* | `get_weather({"city":"北京"})` 各线均返回 function_call |
| 5 | 思考等级正常，不出现 400 | ✅ 通过* | low/high 全过，无 400 |
| 6 | 长上下文行为正常 | ✅ 通过 | 36,023 字符输入，HTTP 200，7.8s 正确应答 |
| 7 | cc-switch 用量统计有数据 | ✅ 通过 | `proxy_request_logs` 4,770 行，含 token 与费用（如 dom 请求 22,024 in / 84 out / $0.0033） |
| 8 | hub-go 日志确认请求进入对应线路 | ✅ 通过 | `converter-{dom,intl,qoder,trae}.log` 按线路记录每次请求；cc-switch forwarder 日志显示正确目标端口 |
| 9 | 停掉某条线路后只有对应模型失败 | ✅ 通过 | 停 Dom(:8787)：`ccs-dom/*` 两个模型 502（0.0s，路由正确），`ccs-qoder/*`、`ccs-trae/*` 照常成功；恢复后 Dom 立即回绿 |
| 10 | cc-switch 退出/重启后行为符合预期 | ✅ 通过 | 本次重启后日志：`codex attach：聚合 → 聚合，路由 hubgo-intl，客户端配置已重写`，:15721 自动恢复，模型目录自动重建 |
| 11 | 老 `:8790` 仍可作为回退 | ✅ 通过 | `dom.glm-5.3-flash` 1.8s、`qoder.Qwen3.8-Flash` 1.4s 均 200；`intl.deepseek-v4.1-flash` 受同一网络问题影响（见下） |
| 12 | 真实 Codex 端到端 | ✅ 通过 | `codex exec -m ccs-dom/glm-5.3-flash` exit=0，正确返回，tokens used 10,869 |

### 本轮重跑明细（12:10–12:17，服务重启后）

```
Dom   聚合  ccs-dom/glm-5.3-flash        通过 5/5（非流式 2.4s / 流式 / 工具 / 思考 low+high）
Dom   聚合  ccs-dom/deepseek-v4-flash    通过 5/5
Qoder 聚合  ccs-qoder/Qwen3.8-Flash      通过 5/5
Trae  聚合  ccs-trae/glm-5.2             通过 5/5
Zcode 聚合  ccs-zcode/glm-5.3            0/5（线路未启动，见阻塞项 B）
Intl  默认  deepseek-v4.1-flash / hy4-preview  当前不可达（见阻塞项 A）
长上下文（36,023 字符 → ccs-dom/glm-5.3-flash）  通过 7.8s
真实 Codex CLI 端到端                       通过 exit=0
```

原始输出：`.dsh-work/verify-route-result3.txt`、`.dsh-work/dom-stop-test.txt`。

## 三、阻塞项（均非配置问题）

### A. Intl 线路：上游 www.workbuddy.ai 网络不可达（临时）

- 现象：`ccs-*`/默认模型打到 :8799 后挂起，hub-go 日志 `Post "https://www.workbuddy.ai/v2/chat/completions": context canceled`。
- 定位：DNS 正常（43.160.158.125），**TCP 443 连接超时**（连续 3 次 ×8s 全超时）；同期 `copilot.tencent.com` 0.03–0.56s 正常。换 Intl 凭据（<凭证目录C>→<凭证目录D>）无效。
- 时间线：11:43–11:44 Intl 全绿（该时段验证清单 30/35，唯一失败是 Zcode）；11:59 起不可达。
- 影响范围：仅 Intl 两个模型；Dom/Qoder/Trae 不受影响；老 `:8790` 的 intl slug 同样受影响（同一上游）。
- 处置：网络恢复后无需改任何配置，重跑 `verify_route.py deepseek` 与 `hy4` 即可。建议观察是否为持续性的国际出口问题。

### B. Zcode 线路：缺 Z.ai API Key（用户决定暂缓）

- 现象：`ccs-zcode/*` 全部 502 `上游连接失败`——:8796 未监听。
- 原因：hub-go 数据根下无 `zcode_api_key.txt`，控制台 `/api/zcode/start` 拒绝启动（不会猜测或伪造凭据）。
- **用户决定（2026-10-13）**：Zcode 当前没有免费额度，需等周末领取免费额度后再补，暂时不管。聚合名单中保留 HubGo Zcode 成员，不移除。
- 后续动作（周末额度到手后）：`POST /api/zcode/key {"key":"..."}` 落盘 → `POST /api/zcode/start` → 重跑 `python3 verify_route.py ccs-zcode` 补齐 5 项验证。

## 四、发现的问题（留给第二阶段 fork 处理）

1. **Codex 警告 `disable_response_storage is ignored`**：cc-switch 改写 `~/.codex/config.toml` 时写入了已废弃的 `disable_response_storage = true` 键。功能无影响，但新版本 Codex 每次启动都会告警。建议 fork 时在 `build_codex_settings`（`src-tauri/src/deeplink/provider.rs`）中移除该键或做版本适配。
2. `cc-switch-model-catalog.json` 中 `auto_compact_token_limit` 统一为 900000（Trae 为 180000），与供应商里配置的 contextWindow（1,000,000 / 200,000）不完全一致——压缩触发线比声明窗口低，行为上偏保守，无阻塞。
3. Intl 线路两个账号（user@example.com、alice）今天有过 `deepseek-v4.1-flash` 用量限流（6004），另 bob@example.com 有账号级风控（11140，至 12:29）。与本次迁移无关，但排障时容易混淆。

## 五、复现与运维命令

```bash
# 启动顺序（hub-go 必须先起）
cd /home/<user>/opt/AI_test/hub-go && nohup ./hub-go/bin/aihub -no-browser &
# 线路启停（Trae/Zcode 需要时）
curl -X POST http://127.0.0.1:8801/api/{dom,intl,qoder,trae,zcode}/start
# cc-switch（按 live-state.json 自动重连聚合）
nohup /usr/bin/cc-switch &
# 全量验证（在 .dsh-work 下）
python3 verify_route.py            # 全部 35 项
python3 verify_route.py ccs-dom    # 只测某线路
# 回退到老方案
cp /home/<user>/opt/AI_test/hub-go/docs/codex-config.toml.bak-20261013-aihub-8790 ~/.codex/config.toml
```

## 六、第二阶段：fork 定制版「Hub Switch」（2026-10-13 完成）

### 6.1 构建产物

- deb：`.dsh-work/hub-switch_4.0.10_amd64.deb`（17MB，包名 `hub-switch`，版本 4.0.10）
- 源码：`/home/<user>/opt/AI_test/cc-switch-main`，已纳入 git（7 个提交，基线 `a70b0c2` 可随时回退）
- 原版 cc-switch 已卸载；原版 deb 备份在 `.dsh-work/cc-switch-original_4.0.10_amd64.deb`（回退用）
- 两包文件零重叠（`/usr/bin/hub-switch` vs `cc-switch`、独立桌面入口与图标），数据目录共用 `~/.cc-switch`（配置无缝继承）

### 6.2 fork 改动清单

| # | 改动 | 内容 |
|---|---|---|
| 1 | 改名与标识 | productName `Hub Switch`、identifier `com.hubswitch.desktop`、mainBinaryName `hub-switch`；i18n（4 语言）、托盘、自启动名、Claude Desktop profile 名同步改名 |
| 2 | 新图标 | 绿底「枢纽-分发」图形（中心圆 + 6 辐条 + 端点），全尺寸 PNG/ICO/ICNS + 预设图标 `hubgo` |
| 3 | HubGo 预设 | `src/config/hubgoProviderPresets.ts`：5 条线路预设在「添加供应商」置顶，端点/模型映射/上下文窗口/思考档位按实测预填，apiFormat=openai_chat |
| 4 | 界面精简 | 侧边栏默认只显示 Claude/Codex；其余 8 个应用在设置里可加回（未删功能） |
| 5 | 移除更新器 | 不再检查 dl.ccswitch.io / 原版 GitHub 发布（fork 用原作者签名公钥无意义）；构建期不再生成签名产物 |
| 6 | 备份兼容 | SQL 备份导入同时接受新旧文件头（`-- Hub Switch SQLite 导出` / `-- CC Switch SQLite 导出`），原版导出的旧备份可导入 |

### 6.3 fork 版验证（2026-10-13 13:32 起）

启动即自动重连聚合（日志：`codex attach：聚合 → 聚合，路由 hubgo-intl，客户端配置已重写`），第一阶段全部配置继承无损。在 fork 版上重跑：

```
Dom   聚合  ccs-dom/glm-5.3-flash        通过 5/5
Dom   聚合  ccs-dom/deepseek-v4-flash    通过 5/5
Qoder 聚合  ccs-qoder/Qwen3.8-Flash      通过 5/5
Trae  聚合  ccs-trae/glm-5.2             通过 5/5
Zcode 聚合  ccs-zcode/glm-5.3            0/5（线路未启动，见阻塞项 B，等周末额度）
Intl  hy4-preview                        通过 5/5
Intl  deepseek-v4.1-flash                6/10（4 次 HTTP 429 上游账号配额耗尽，
                                          上游称 18:50 恢复；国际线多凭证，其余请求已自动走可用账号）
真实 Codex CLI 端到端                     exit=0，tokens used 10,861
Codex /model 目录（8 模型）               名称与第一阶段调整后一致
Codex config.toml                        model_provider=custom → http://127.0.0.1:15721/v1 ✓
```

fork 品牌已在运行时生效：代理错误信息为「Hub Switch local proxy failed…」，供应商显示名为「国际」「国内」。

### 6.4 最后阶段：hub-go UI 重构（2026-10-13 完成）

按交接文档「hub-go UI 重构目标」执行，并结合实际演进调整：**聚合管理以 Hub Switch 为准，hub-go 不做重复功能**（用户拍板），**Token 用量统计撤下**（Hub Switch 已自带，不重复）。

结构（侧边栏分页，非手风琴）：

| 页面 | 内容 |
|---|---|
| 仪表盘 | 网关健康（Hub Switch :15721 只读状态 + 旧 :8790）、快捷签到（徽章胶囊分组）、服务状态总览（紧凑瓦片） |
| 服务与凭证 | 完整服务卡片：当前凭证摘要（「凭证 2/4 可用」+ 风控红点）、点击展开凭证抽屉（单选切换/限流到几点/余额/风控原因）、风控凭证浅红背景+左侧红条、已停止线路整卡置灰（opacity .6）、「仅显示已启动」过滤、主次按钮分明（核心操作蓝、辅助文本按钮） |
| 聚合与网关 | Hub Switch 聚合状态只读展示（在线/模式/默认供应商/Stack 成员/聚合模型表：模型 slug、显示名、所属线路）+ 旧 :8790 配置折叠为「回退专用」区（保留白名单增删与上下文上限，附回退方法说明） |

新增 `/api/gateway` 端点：探测 :15721 TCP + 读 `~/.cc-switch/live-state.json` 与模型目录（best-effort，读不到只报端口状态）。

构建部署：Go 1.27.2 重编 `bin/aihub`（旧版备份 `aihub.prev2`），go vet + 38s 单测通过。浏览器实测三页渲染、凭证抽屉、风控高亮、过滤开关、聚合模型表均正常；端到端冒烟 4/4 通过。截图留档 `.dsh-work/ui-{dash,svc,agg,agg-open,cred-drawer}.png`。

后续唯一剩余项：`:8790` 退役（默认不自启 → UI 折叠 → 观察后删代码），建议观察数日稳定后再动。

## 七、结论

- **第一阶段（原版 cc-switch 聚合验证）通过**，**第二阶段（fork 定制 Hub Switch）完成并验证通过**。
- 全链路现状：Codex → Hub Switch :15721 → hub-go 各线路 → 上游，Dom/Qoder/Trae/Intl(hy4) 全绿，真实 Codex CLI 端到端 exit=0。
- 遗留两项均为上游/账号侧条件，与迁移无关：
  1. Zcode：等周末 Z.ai 免费额度，提供 Key 后补跑 5 项；
  2. Intl `deepseek-v4.1-flash`：账号配额 429，上游称 2026-10-13 18:50 恢复，届时自动恢复。
- 最后阶段（未开始）：`:8790` 退役与 hub-go UI 重构，建议观察数日稳定后再动。
