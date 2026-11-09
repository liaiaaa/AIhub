# 2026-11-09 Codex 混合模型目录设计

> 状态：设计稿，已获用户口头批准，待用户审阅本规格文件后进入实施计划。
> 适用范围：`<项目根>`（aihub 网关，本机为 `/home/<user>/opt/AI_test/hub-go`）+ `~/.codex/config.toml`（Codex 客户端）。
> 设计原则：照 magpie 主链路，官方推荐模型列表按当前登录账号实时获取，不硬编码官方模型名单；第三方模型仍由用户在「目录模型」里手动添加。

## 1. 背景与问题

用户目标（m06541）：让 Codex 的同一个 `/model` 选择器里同时出现 **Codex 官方模型** 与 **aihub 接入的第三方模型**，效果对齐 magpie；现在只能看到自己在聚合白名单里手动添加的 7 个第三方模型。

只读实测结论（2026-11-08）：

- `~/.codex/config.toml` 当前是静态目录接管形态：顶层 `model_provider = "aihub"`、`model_catalog_json = "~/.codex/aihub-model-catalog.json"`；`[model_providers.aihub] base_url = "http://127.0.0.1:8790/v1"`、`wire_api = "responses"`、`requires_openai_auth = true`。
- `~/.codex/aihub-model-catalog.json`、`GET :8790/v1/models`、`GET :8790/v1/codex/models` 都只有 7 条第三方模型，官方样式 slug 0 条。
- Codex 桌面版只在启动时读一次 `model_catalog_json`；那一刻 aihub 拿不到请求里的 `Authorization` / `chatgpt-account-id`，所以无法按账号去问官方后端。
- 官方透传路由仍在：`hub-go/proxy_official.go:47 func (h *Hub) officialPassthrough(w http.ResponseWriter, r *http.Request, nb []byte)` 把 `gpt-*` responses 原样转 `https://chatgpt.com/backend-api/codex/responses`；不带凭据实测 `POST :8790/v1/responses {"model":"gpt-5.2"}` 返回 `HTTP 401 {"type":"auth_missing", ...}`，说明路由活着但选择器没有入口。
- magpie 主链路（[codex_backend.go](https://raw.githubusercontent.com/yetone/magpie/main/internal/gateway/codex_backend.go)）：`const CodexPath = "/backend-api/codex"`；`GET /backend-api/codex/models` 由 `s.codexModels(w, r)` 处理；`codexListKey` 用 `chatgpt-account-id`（缺失时用 `Authorization`）+ `RawQuery` 做按账号缓存 key；`askCodexList` 带当前请求头转发 `provider.CodexBase + "/models"`（`https://chatgpt.com/backend-api/codex`），3 秒等不到回退缓存；拿到官方列表后 `codexMerged(own, cached)` 追加 magpie 模型。
- magpie 配置写入点（[codex.go](https://github.com/yetone/magpie/blob/main/internal/agent/codex.go)、[wsl.go](https://github.com/yetone/magpie/blob/main/internal/agent/wsl.go)）：`edit.SetTOMLTop(path, edit.KV{Path: "openai_base_url", Value: at.codexURL()})`；`func (p place) codexURL() string { return p.gw() + gateway.CodexPath }`。[中文说明](https://github.com/yetone/magpie/blob/main/site/public/docs/zh/codex-models.html)：登录 ChatGPT 时保留 Codex 自带 OpenAI provider，用 `openai_base_url` 指向网关，官方模型仍用账号发给 OpenAI，magpie 模型加在旁边；未登录/API Key 登录才写本地目录文件。
- 本机 Codex `/usr/lib/chatgpt/resources/codex --version` = `codex-cli 0.167.0-alpha.3.2`；二进制里 `model_catalog_url` 19 次、`openai_base_url` 12 次、`chatgpt-account-id` 7 次，配置项存在但当前 config.toml 未启用 `openai_base_url`。

## 2. 成功标准

1. 用户重开 Codex 后，`/model` 同屏出现「官方推荐模型」与「aihub 第三方模型」。
2. 官方列表来自当前登录账号的实时后端响应；切换账号后列表随账号变化，aihub 不维护也不猜测官方模型名单。
3. 选官方模型发消息：请求经 aihub 原样透传到官方后端，登录态由 Codex 自带 provider 提供。
4. 选第三方模型发消息：仍走现有 `aggDispatch(w, r, "responses")` 与线路代理，行为不改。
5. 现有 `/v1/*` 对 DSH、Claude、其它 OpenAI 兼容客户端的行为不变。
6. 「恢复官方」能把 config.toml 逐字节还原到接管前；任何失败都有备份可回退。

## 3. 方案选择（已确认）

用户已在 ask_user_question 中选 **A 照 magpie 主链路做（推荐）**。被否的备选：

- B 静态目录合并 `~/.codex/models_cache.json`：改动小，但缓存 `client_version=0.162.0`、`fetched_at=2026-10-22`，缺 `gpt-5.2` / `gpt-6.1-sol`，不保证按账号实时。
- C `model_catalog_url` 动态 HTTP 目录 + 缓存：Codex 优先读 `model_catalog_json`，本机 0.167 行为还要另测，且仍不是按账号实时。

## 4. 架构与配置形态

### 4.1 Codex 配置（由 aihub「接管」按钮改）

备份 `~/.codex/config.toml` 为 `config.toml.bak-hubgo-<时间戳>` 后：

- 顶层新增 `openai_base_url = "http://127.0.0.1:8790/backend-api/codex"`，行上方写 `# aihub:` 注释说明用途与删除方法。
- 顶层移除 `model_provider`、`model_catalog_json`。理由：magpie 登录态模式保留 Codex 自带 OpenAI provider；残留 `model_catalog_json` 会让桌面版优先读本地静态文件，覆盖后端返回的混合列表。
- 保留 `model`、`[model_providers.aihub]`、`[profiles.official]`、`[profiles.aihub]` 作为应急回退；不改 `[shell_environment_policy]` 等用户配置。
- **不读、不改、不落盘 `~/.codex/auth.json`**。Codex 自己从登录态注入 `Authorization` / `chatgpt-account-id`。

### 4.2 aihub 新增端点（只在聚合口 8790）

- 现有 `/v1/*` 一字节不动。
- 新增前缀 `/backend-api/codex/`，首版两个端点：`GET /backend-api/codex/models`、`POST /backend-api/codex/responses`。
- 复用 `hub-go/proxy_official.go` 的 `officialPassthrough`，并从 `hub-go/proxy_agg.go` 的 `resolveAggModel` 前三级抽出 `resolveAggThirdParty(slug) (aggModel, bool)`（白名单 / `ccs-` / `<线路>.<模型>`）；完整 `resolveAggModel` 及其 `gpt-*` 官方兜底仍只服务现有 `/v1/*`，行为不变。
- `hub-go/proxy_official.go:27 var officialCatalogModels` 是零调用残留，本期删除；`proxy_agg.go:201 var aggPrettyNames` 是第三方模型友好名的通用表（`aggDisplayName` 在生产路径引用），**不得删除**，其中的官方显示名条目只在实测确认无引用后单独清理，不随本期改动。

### 4.3 关键前置验证（实施第 0 步）

**唯一未实测假设**：本机 Codex `0.167.0-alpha.3.2` 在「有 `openai_base_url`、无 `model_provider`」时，是否把 `/models` 与 `/responses` 都发到 `/backend-api/codex/*`。实施计划第一步必须用临时 `CODEX_HOME` + 本地假后端实测请求落点；与 magpie 一致才继续，不一致就停下报告并修订本规格，不猜着做。

## 5. 数据流

### 5.1 `GET /backend-api/codex/models`

1. 缓存 key = `chatgpt-account-id`（缺失时 `Authorization`）+ `r.URL.RawQuery`，按账号隔离；缓存有效期 1 分钟，硬性拉取超时 3 秒，后台最长 30 秒。
2. 带当前请求的登录态头（至少 `Authorization`、`chatgpt-account-id`、官方透传需要的 `X-*`）转发 `GET https://chatgpt.com/backend-api/codex/models`；官方响应体限读 16 MiB。
3. 官方条目原样保留全部字段，只删 `availability_nux` / `upgrade`（照 `codexcat.ownEntry`）；不改官方 slug、显示名、推理档位、上下文窗口、可见性。合并完成后按同一账号 key 缓存「官方 slug 集合」，供 responses 分流使用；集合随目录缓存同生共死，不落盘、不打印。
4. 把 `aggSnapshot()` 的第三方模型转成 Codex 目录条目追加在后；官方优先；priority 连续编号并保证桌面版可读的前 100 条内不被挤掉。
5. 回 `{"models":[...]}`；ETag 打 `+aihub-` 标记，避免 Codex 因官方 ETag 长期不刷新合并结果。
6. 日志只记账号 key 的哈希/状态码/条目数，**绝不记 Authorization、chatgpt-account-id 原值、token 或密钥**。

### 5.2 `POST /backend-api/codex/responses`

1. 读 body 的 `model`，并用 `resolveAggThirdParty(model)` 判定是否第三方。
2. 第三方命中 → `aggDispatch(w, r, "responses")`，行为与现有 `/v1/responses` 一致。
3. 第三方未命中且该 slug 在当前账号缓存的「官方 slug 集合」里 → 复用 `officialPassthrough` 原样转 `https://chatgpt.com/backend-api/codex/responses`，沿用客户端 Authorization、自动补 `store=false`、流式回传。判定顺序保证按账号下发的 `codex-auto-review`、`gpt-reserve` 以及未来不带 `gpt-` 前缀的官方模型都走官方；不依赖 `isOfficialModelSlug()` 的 `gpt-` 前缀硬编码。
4. 第三方与官方集合都不命中 → 返回结构化 `404 / unknown_model`，不猜测路由、不默认透传；`isOfficialModelSlug` 只保留给现有 `/v1/*` 的兼容分支。
5. 官方目录缓存缺失（例如 Codex 直接发帖、未先拉 `/models`）→ 以 `GET /backend-api/codex/models` 的同一套逻辑同步取一次官方列表后再判定；取不到时仍按上述规则报错，不把未知模型悄悄发给官方。
6. 请求体过大/解析失败时返回结构化错误；不把请求体或凭据写进日志。

### 5.3 非目标（YAGNI）

本期不做：`/responses/compact`、WebSocket responses、多账号池、账号路由、`codex/` 前缀命名空间、路由组、额度共享、官方模型用量审计、把官方缓存写进仓库、修改 DSH/Claude 配置。

## 6. 组件边界

| 单元 | 职责 | 依赖 |
|---|---|---|
| `hub-go/codex_backend.go`（新文件） | 官方目录按账号拉取/短缓存/官方 slug 集合/合并、responses 分流、ETag 标记 | `proxy_official.go`、`resolveAggThirdParty`、`aggDispatch`、`aggSnapshot()` |
| `hub-go/client_takeover.go` | 「混合接管 / 还原」：备份、保注释 TOML 顶层编辑、状态返回 | 现有 TOML 文本编辑 helper、`client_profile.go` |
| `hub-go/proxy_agg.go` | 在聚合口注册两个 `/backend-api/codex/*` 路由 | 现有 mux |
| `hub-go/codex_catalog.go` | 用现有 `codexModelEntry` / 模板生成第三方目录条目，供合并函数复用 | `aggSnapshot()` |
| `hub-go/assets/console.html` | 客户端接管卡片的文案与状态判定改为 `openai_base_url` 口径 | `/api/clients/status` |

每个单元只做一件事：`codex_backend.go` 不写 Codex 配置，`client_takeover.go` 不碰官方 HTTP 请求，`codex_catalog.go` 不负责缓存。

## 7. 错误处理与护栏

- 官方 `/models` 3 秒不回或网络失败 → 回退只读 `~/.codex/models_cache.json` 的官方条目（不打印 identity/etag 值），第三方照常追加，日志记「官方目录回退缓存」。
- 官方返回 401/403 → 状态与错误体原样回给 Codex，让它自己刷新登录态；日志只记状态码。
- 缓存也没有 / 官方列表为空 → 只回第三方且保证非空；空表会让整个 Codex 目录加载失败（知识库 `codex模型目录被go-test写空.md` 已踩过）。
- 官方 responses 透传失败 → 原样回传状态与可读错误，不伪装成第三方错误、不自动换线路。
- 合并后的 JSON 必须能被 `json.Unmarshal` 解析；响应体硬上限 1 MiB（`codexCatalogMost = 1 << 20` 同类护栏），条目上限 200；超限按「官方 list 在前 → 官方 hide → 第三方」截断，且截断后 `len(models)>0`。
- 配置写入前后都备份；TOML 写后用 `python3 tomllib` 解析确认合法；解析失败自动从备份还原。
- 「恢复官方」从最近的 `config.toml.bak-hubgo-<时间戳>` 逐字节还原；备份列表只读展示，不自动删除。
- 所有新 HTTP 客户端走 `backendClient` / `directClientFor` 现有封装，不裸用 `http.Client`（仓库有 `TestNoBareHTTPClientOnRemotePaths` 门禁）。
- 不重启承载当前会话的 dsh 进程；构建/重启 aihub 需用户按给的明确命令执行，因为本会话模型链路是 dsh→:8790 agg→:8797 qoder。

## 8. 测试策略（TDD）

先写失败测试、看到 RED，再最小实现 GREEN。

### 8.1 单元测试（httptest 假官方后端）

- 两个账号（不同 `chatgpt-account-id`）返回不同官方 models → 缓存互不串号。
- 官方条目全部字段原样保留，`availability_nux` / `upgrade` 被删。
- 第三方条目追加在后，priority 连续，前 100 条包含全部官方 `visibility=list` 条目。
- ETag 带 `+aihub-` 标记。
- 官方 3 秒超时 → 回退缓存；401/403 → 原样状态。
- 无官方也无缓存 → 只回第三方且 `len(models)>0`。
- 响应体超过 1 MiB 或条目超过 200 → 截断后仍含官方 `visibility=list` 条目，且 `len(models)>0`。
- `POST /backend-api/codex/responses`：`gpt-5.2` 与 `codex-auto-review`（非 `gpt-` 前缀的官方 slug）都走假官方上游；`qoder.DeepSeek-Flash` 走 fake 线路；未知 slug 返回 `404 unknown_model` 且不打任何上游；各断言一次且不互相串。
- 官方目录缓存缺失时发 responses → 同步拉一次官方列表后再分流；拉不到则不透传。
- 日志断言：不含 `Authorization`、不含账号头原值、不含 token。

### 8.2 配置测试（临时 `CODEX_HOME`）

- 接管后顶层有 `openai_base_url`、无 `model_provider` / `model_catalog_json`，`[profiles.official]` 与用户注释仍在。
- 备份文件生成且与接管前内容逐字节一致。
- 还原后 config.toml 与接管前逐字节一致；`auth.json` 字节不变。
- TOML 写入后 `python3 tomllib` 可解析。

### 8.3 回归与验收

- `go test ./... -count=1`、`go vet ./...`、`gofmt -l`、`scripts/build.sh` 全绿。
- 预览实例（临时数据根 + 非生产端口）用 `curl` 带假 Authorization 打两个新端点，确认合并目录与分流。
- 现有 `/v1/models`、`/v1/responses`、`/v1/codex/models` 行为不变（回归断言）。
- 真实验收（用户重开 Codex 后）：`/model` 同屏出现官方推荐 + aihub 第三方；各选一个发消息成功；`codex --profile official` 仍能直连。设计阶段**不消耗官方额度**：带真实登录态的完整 `gpt-5.2` 请求只在用户验收时产生，本规格不要求 agent 主动发。

## 9. 交付顺序（供实施计划展开，不在本规格里实现）

1. 实施第 0 步：临时 `CODEX_HOME` 实测 Codex 0.167 对 `openai_base_url` 的请求落点；不通过就停。
2. 写测试并实现 `codex_backend.go` 的 models 合并与 responses 分流。
3. 写测试并改 `client_takeover.go` / `assets/console.html` 的混合接管、状态与还原。
4. 删 `officialCatalogModels` 零调用残留常量，跑全量回归。
5. 构建 bin/aihub、PII 扫描、提交；给用户明确的「重开 Codex」与「必要时重启 aihub」命令。
6. 用户重开 Codex 验收；失败按回滚清单恢复备份。

## 10. 已核实但未改的事实（供实施者核对）

- `hub-go/client_takeover.go:123 takeOverCodex()`、`:273 restoreCodex()`、`:393 apiClientsTakeover()`、`:366 apiClientsRestore()`；`hub-go/main.go:128-131` 注册三个 `/api/clients/*`；`hub-go/assets/console.html:2591` 当前用 `d.codex_provider === 'aihub'` 判定已接管，需改为 `openai_base_url` 口径。
- `hub-go/proxy_agg.go:501 func resolveAggModel(slug string) (aggModel, bool)` 当前前三级匹配白名单 / `ccs-` / `<线路>.<模型>`，最后 `:533` 用 `isOfficialModelSlug(slug)` 把 `gpt-*` 解析成 `aggModel{Line:"official"}`；新链路只抽前三级为 `resolveAggThirdParty`，现有 `/v1/*` 的官方兜底保持不变。
- `hub-go/proxy_agg.go:324` 注册 `/v1/codex/models`；`:767` 调 `officialPassthrough`；`hub-go/proxy_official.go:23 const officialBackendURL = "https://chatgpt.com/backend-api/codex/responses"`；`hub-go/proxy_agg.go:201 var aggPrettyNames` 由 `aggDisplayName` 引用，不是可删残留。`hub-go/proxy_agg.go:729 func (h *Hub) aggDispatch(w http.ResponseWriter, r *http.Request, kind string)` 是第三方 responses 入口。
- 当前线上 `aihub-console` 已于 `2026-11-09 08:54:46` 恢复；仓库 HEAD `4a6053e`、工作区 0 改动、与远端一致。
- magpie `codexMerged` / `codexListKey` / `codexcat.FitOwn` / `Keep` 的结论来自 GitHub raw 源码，仅作设计参考，不复制其代码；aihub 采用自己的实现与测试。
