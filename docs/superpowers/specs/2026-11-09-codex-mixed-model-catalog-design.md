# 2026-11-09 Codex 混合模型目录设计（修订 A′）

> 状态：设计稿 **修订 A′**，已获用户选定方案；待用户审阅本修订稿后进入实施计划。
> 适用范围：`<项目根>`（aihub 网关，本机为 `/home/<user>/opt/AI_test/hub-go`）+ `~/.codex/config.toml` / `~/.codex/aihub-model-catalog.json`（Codex 客户端）。
> 设计原则：**本地混合目录 + responses 转发**。Codex 的模型选择器读本地 `model_catalog_json`；aihub 把「本机 Codex 内置官方目录」与「用户手选的第三方模型」合并写进这份文件，并让所有 responses 经 aihub 按 slug 分流。不再追求按账号实时拉取官方目录。

## 1. 背景与问题

用户目标（m06541）：让 Codex 的同一个 `/model` 选择器里同时出现 **Codex 官方模型** 与 **aihub 接入的第三方模型**，效果对齐 magpie；现在只能看到自己在聚合白名单里手动添加的 7 个第三方模型。

只读实测结论（2026-11-08 ~ 11-09）：

- `~/.codex/config.toml` 当前是静态目录接管形态：顶层 `model_provider = "aihub"`、`model_catalog_json = "~/.codex/aihub-model-catalog.json"`；`[model_providers.aihub] base_url = "http://127.0.0.1:8790/v1"`、`wire_api = "responses"`、`requires_openai_auth = true`。
- `~/.codex/aihub-model-catalog.json`、`GET :8790/v1/models`、`GET :8790/v1/codex/models` 都只有 7 条第三方模型，官方样式 slug 0 条。
- 官方透传路由仍在：`hub-go/proxy_official.go:47 func (h *Hub) officialPassthrough(w http.ResponseWriter, r *http.Request, nb []byte)` 把官方 responses 原样转 `https://chatgpt.com/backend-api/codex/responses`；不带凭据实测 `POST :8790/v1/responses {"model":"gpt-5.2"}` 返回 `HTTP 401 {"type":"auth_missing", ...}`，说明路由活着但选择器没有入口。
- magpie 主链路（[codex_backend.go](https://raw.githubusercontent.com/yetone/magpie/main/internal/gateway/codex_backend.go)）：`const CodexPath = "/backend-api/codex"`；`GET /backend-api/codex/models` 由 `s.codexModels(w, r)` 处理；`codexListKey` 用 `chatgpt-account-id` + `RawQuery` 做按账号缓存 key；`askCodexList` 带当前请求头转发 `provider.CodexBase + "/models"`，3 秒等不到回退缓存；拿到官方列表后 `codexMerged(own, cached)` 追加 magpie 模型。
- magpie 配置写入点（[codex.go](https://github.com/yetone/magpie/blob/main/internal/agent/codex.go)、[wsl.go](https://github.com/yetone/magpie/blob/main/internal/agent/wsl.go)）：`edit.SetTOMLTop(path, edit.KV{Path: "openai_base_url", Value: at.codexURL()})`；`func (p place) codexURL() string { return p.gw() + gateway.CodexPath }`。[中文说明](https://github.com/yetone/magpie/blob/main/site/public/docs/zh/codex-models.html)：登录 ChatGPT 时用 `openai_base_url` 指向网关，官方模型仍用账号发给 OpenAI。
- 本机 Codex `/usr/lib/chatgpt/resources/codex --version` = `codex-cli 0.167.0-alpha.3.2`。

### 1.1 第 0 步前置验证（已完成，推翻方案 A）

规格初版第 4.3 节要求先实测「Codex 0.167 是否把 `/models` 打到 `openai_base_url`」。用**临时 `CODEX_HOME` + 本地假后端**（工程内 `.tmp/codex-url-probe/`，未改生产配置、未读 `auth.json`、未消耗官方额度）实测：

- `codex exec -c 'openai_base_url="http://127.0.0.1:8899/backend-api/codex"' ping`：假后端收到 `responses_hits=13`、`models_hits=0`。**responses 走 `openai_base_url`，`/models` 不走。**
- `codex debug models` 指向假后端：`(无请求)`，输出仍是内置目录 `744428 B`。
- 配置 `model_catalog_url` 时 app-server 明确报错 `model_catalog_url is ignored`。
- app-server JSON-RPC `model/list`（含 `experimentalApi=true`）：`(无请求)`。配 `model_catalog_json` 本地样例时返回文件里的 `probe-from-file`；删掉该配置则返回内置 `gpt-6.1-sol`；`models_hits=0`。
- `model_catalog_json` 格式必须是完整条目；最小样例缺 `description` 报 `failed to parse model_catalog_json ... missing field 'description'`。

**结论**：本机 0.167 的模型选择器目录来自 `model_catalog_json` 或内置 bundled catalog，不向 `openai_base_url` 的 `/models` 发请求；因此「按账号实时拉官方 `/models`」在本机客户端上不成立。按规格要求停下报告，用户在 ask_user_question 中选 **A′ 本地混合目录 + responses 转发**。

### 1.2 A′ 可行性实测（已完成）

- `codex debug models --bundled`：输出 `{"models":[12 条]}`，744428 B，slug 含 `gpt-6-astra`、`gpt-6.1-sol`、`gpt-6.1-terra`、`gpt-6-sol`、`gpt-6-luna`、`gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna`、`gpt-daybreak-blue-latest`、`gpt-daybreak-red-latest`、`gpt-5.5`、`codex-auto-review`；每条 42 字段（含 `description` 等），`visibility` 为 `list` 或 `hide`。
- 把 bundled 12 条 + 现有 `~/.codex/aihub-model-catalog.json` 7 条第三方直接拼成 `{"models":[19 条]}`（约 1.07 MB）：`codex debug models` 读到 `official=1 third=1`；app-server `model/list` 读到 `official=2 third=2`，均成功。
- 同份配置加 `openai_base_url` 指向假后端：
  - `exec -m dom.deepseek-v4-flash` → 6 次 `POST /backend-api/codex/responses`，body `{"model":"dom.deepseek-v4-flash",...}`。
  - `exec -m gpt-6.1-sol` → 6 次 `POST /backend-api/codex/responses`，body `{"model":"gpt-6.1-sol",...}`，`authorization_present=True`（值不打印）。

**结论**：A′ 可同屏显示，且官方/第三方 responses 都落到 aihub 的 `/backend-api/codex/responses`，model 名保持原样；aihub 只需按 slug 分流。

## 2. 成功标准

1. 用户重开 Codex 后，`/model` 同屏出现「官方模型」与「aihub 第三方模型」。
2. 官方条目来自本机 Codex 内置 bundled 目录快照（不按账号实时、不硬编码名单）；用户点一次「接管 / 刷新目录」可跟进 Codex 升级带来的新官方模型。**切账号不自动改变官方列表，明确列为非目标。**
3. 选官方模型发消息：请求经 aihub `/backend-api/codex/responses` 分流到 `officialPassthrough`，沿用客户端登录态原样转官方后端。
4. 选第三方模型发消息：经同一端点分流到现有 `aggDispatch(w, r, "responses")` 与线路代理，行为不改。
5. 现有 `/v1/*` 对 DSH、Claude、其它 OpenAI 兼容客户端的行为不变。
6. 「恢复官方」能把 `config.toml` 逐字节还原到接管前；任何失败都有备份可回退；`auth.json` 字节不变。

## 3. 方案选择（已确认）

- 初版 **A 照 magpie 主链路**：被第 0 步前置验证推翻（本机 0.167 不打 `/models`），不再采用。
- 用户选定 **A′ 本地混合目录 + responses 转发（推荐）**：保留 `openai_base_url` 让 responses 经 aihub；aihub 生成混合 `model_catalog_json`（bundled 官方条目 + 第三方条目）。能同屏，但官方目录不是按账号实时。
- 被否备选：**B 坚持按账号实时**（aihub 读 `auth.json` 自己拉官方 `/models` 再写目录）——需明确授权读登录态，风险更高，用户未选；**C 暂停实现**——保留规格等客户端支持，用户未选；旧 B 静态 `models_cache.json`、旧 C `model_catalog_url` 同样被本机实测否定（缓存旧 / 配置项被忽略）。

## 4. 架构与配置形态

### 4.1 Codex 配置（由 aihub「接管」按钮改）

备份 `~/.codex/config.toml` 为 `config.toml.bak-hubgo-<时间戳>` 后：

- 顶层新增（或改写）`openai_base_url = "http://127.0.0.1:8790/backend-api/codex"`，行上方写 `# aihub:` 注释说明用途与删除方法。
- 顶层**移除 `model_provider`**：保留 `model_provider = "aihub"` 会让 responses 走 `[model_providers.aihub].base_url`（`/v1/responses`）而不是 `openai_base_url`；实测无 `model_provider` 时 provider 回落 `openai`，responses 正确落到 `/backend-api/codex/responses`。
- 顶层**保留 `model_catalog_json = "<home>/.codex/aihub-model-catalog.json"`**，但文件内容由 aihub 生成/刷新为混合目录（见 5.1）。不再移除它——A′ 下它是同屏显示的唯一入口。
- 保留 `model`、`[model_providers.aihub]`、`[profiles.official]`、`[profiles.aihub]` 作为应急回退；不改 `[shell_environment_policy]` 等用户配置。
- **不读、不改、不落盘 `~/.codex/auth.json`**。Codex 自己从登录态注入 `Authorization` / `chatgpt-account-id`；实测假后端能看到 `authorization_present=True`。

### 4.2 aihub 新增端点（只在聚合口 8790）

- 现有 `/v1/*` 一字节不动。
- 新增 `POST /backend-api/codex/responses`（并允许 `GET` 走同一分流以兼容客户端重试）：这是 A′ 下唯一必需的新 HTTP 端点。
- **不新增** `GET /backend-api/codex/models`：本机 0.167 不请求它；未来客户端支持时再另立规格。也不新增 `model_catalog_url`（被忽略）。
- 分流从 `hub-go/proxy_agg.go` 的 `resolveAggModel` 前三级抽出 `resolveAggThirdParty(slug) (aggModel, bool)`（白名单 / `ccs-` / `<线路>.<模型>`）；完整 `resolveAggModel` 及其 `gpt-*` 官方兜底仍只服务现有 `/v1/*`，行为不变。
- `hub-go/proxy_official.go:27 var officialCatalogModels` 是零调用残留，本期删除；`proxy_agg.go:201 var aggPrettyNames` 是第三方模型友好名的通用表（`aggDisplayName` 在生产路径引用），**不得删除**。

### 4.3 前置验证状态

第 0 步已完成且结论为「方案 A 不成立、A′ 成立」（见 1.1 / 1.2）。实施计划不再重复第 0 步的探测；但任务 1 必须先写一条回归测试固定「移除 `model_provider` 后 responses 落 `/backend-api/codex/responses`」，防止后续配置改动把落点带回 `/v1/responses`。

## 5. 数据流

### 5.1 生成混合 `model_catalog_json`（接管 / 刷新目录时）

1. 取官方条目：调用本机 `codex debug models --bundled`（只读、不联网、不消耗额度、不需要登录态），解析 `{"models":[...]}`。命令路径可由 env `AIHUB_CODEX_BIN` 覆盖，默认探测 `/usr/lib/chatgpt/resources/codex`。
2. 取第三方条目：遍历 `aggSnapshot()`，复用 `hub-go/codex_catalog.go` 的 `codexModelEntry` / `codexModelsResponseFrom` 生成 33+ 键条目（与现有 `writeCodexCatalogFile` 同口径）。
3. 合并顺序：官方 `visibility=list` → 官方 `visibility=hide` → 第三方；priority 连续编号。官方条目原样保留全部字段（不删 `availability_nux` / `upgrade`，因为它是本地文件而非 HTTP 合并响应，实测可直接读）。
4. 写盘：`~/.codex/aihub-model-catalog.json`，tmp + rename，空表拒写；写前备份为 `aihub-model-catalog.json.bak-hubgo-<时间戳>`。
5. 同时把「官方 slug 集合」写入数据根 `codex_official_slugs.json`（仅 slug 列表，0600）并缓存在内存，供 responses 分流；集合随混合目录一起生成，不落凭据、不打印。
6. 失败回退：bundled 命令不可用 / 解析失败 → 改读 `~/.codex/models_cache.json` 的 `models` 作为官方条目；再失败则只写第三方且保证 `len(models)>0`（空表会让整个 Codex 目录加载失败，知识库 `codex模型目录被go-test写空.md` 已踩过）。日志记「官方目录回退」及原因，不记文件内容。
7. 护栏：混合目录文件上限 4 MiB（实测 19 条约 1.07 MB，留足余量）、条目上限 200；超限按上述顺序截断且截断后非空。写后用 `json.Unmarshal` 自检。

### 5.2 `POST /backend-api/codex/responses`

1. 读 body 的 `model`，用 `resolveAggThirdParty(model)` 判定是否第三方。
2. 第三方命中 → `aggDispatch(w, r, "responses")`，行为与现有 `/v1/responses` 一致。
3. 第三方未命中且该 slug 在「官方 slug 集合」里 → 复用 `officialPassthrough` 原样转 `https://chatgpt.com/backend-api/codex/responses`，沿用客户端 Authorization、自动补 `store=false`、流式回传。判定顺序保证 `codex-auto-review`、`gpt-reserve` 等不带 `gpt-` 前缀的官方 slug 也走官方；不依赖 `isOfficialModelSlug()` 的前缀硬编码。
4. 两者都不命中 → 结构化 `404 / unknown_model`，不猜测路由、不默认透传；`isOfficialModelSlug` 只保留给现有 `/v1/*` 的兼容分支。
5. 「官方 slug 集合」缺失（例如首次启动还没生成目录）→ 现场按 5.1 第 1~2 步只读计算一次集合（不写盘），仍取不到则按第 4 条报错，不把未知模型悄悄发给官方。
6. 请求体过大 / 解析失败 → 结构化错误；不把请求体或凭据写进日志。

### 5.3 非目标（YAGNI）

本期不做：`GET /backend-api/codex/models`、按账号缓存/ETag、WebSocket responses、多账号池、账号路由、`codex/` 前缀命名空间、路由组、额度共享、官方模型用量审计、把官方缓存写进仓库、修改 DSH/Claude 配置、读取 `auth.json`。

## 6. 组件边界

| 单元 | 职责 | 依赖 |
|---|---|---|
| `hub-go/codex_backend.go`（新文件） | 生成混合目录（bundled 官方 + 第三方）、维护官方 slug 集合、`/backend-api/codex/responses` 分流 | `codex_catalog.go`、`resolveAggThirdParty`、`aggDispatch`、`officialPassthrough` |
| `hub-go/client_takeover.go` | 「混合接管 / 还原」：备份、保注释 TOML 顶层编辑、生成目录、状态返回 | 现有 TOML 文本编辑 helper、`client_profile.go` |
| `hub-go/proxy_agg.go` | 在聚合口注册 `/backend-api/codex/responses`；抽出 `resolveAggThirdParty` | 现有 mux |
| `hub-go/codex_catalog.go` | 复用 `codexModelEntry` / 模板生成第三方目录条目 | `aggSnapshot()` |
| `hub-go/assets/console.html` | 客户端接管卡片文案与状态判定改为 `openai_base_url` 口径 | `/api/clients/status` |

每个单元只做一件事：`codex_backend.go` 不写 Codex 配置，`client_takeover.go` 不碰官方 HTTP 请求，`codex_catalog.go` 不负责分流。

## 7. 错误处理与护栏

- bundled 目录抽取失败 → 回退 `~/.codex/models_cache.json`；再失败 → 只写第三方且非空。
- 官方 responses 透传失败 → 原样回传状态与可读错误，不伪装成第三方错误、不自动换线路。
- 混合目录写入前后都备份；写后 `json.Unmarshal` 自检；`config.toml` 写后用 `python3 tomllib` 解析确认合法，失败自动从备份还原。
- 「恢复官方」从最近的 `config.toml.bak-hubgo-<时间戳>` 逐字节还原；备份列表只读展示，不自动删除。
- 所有新 HTTP 客户端走 `backendClient` / `directClientFor` 现有封装，不裸用 `http.Client`（仓库有 `TestNoBareHTTPClientOnRemotePaths` 门禁）。
- 日志只记状态码 / 条目数 / 回退原因，**绝不记 Authorization、chatgpt-account-id 原值、token、密钥或请求体**。
- 不重启承载当前会话的 dsh 进程；构建/重启 aihub 需用户按给的明确命令执行，因为本会话模型链路是 dsh→:8790 agg→:8797 qoder。

## 8. 测试策略（TDD）

先写失败测试、看到 RED，再最小实现 GREEN。

### 8.1 单元测试（httptest + 临时数据根）

- 混合目录：注入假 bundled JSON 与假 `aggSnapshot()`，断言官方 `list` 在前、官方 `hide` 居中、第三方在后、priority 连续、官方字段不被改写、`len(models)>0`。
- bundled 不可用 → 回退 `models_cache.json`；两者都无 → 只回第三方且非空。
- 混合目录超过 4 MiB / 200 条 → 截断后仍含官方 `visibility=list` 且非空；写盘 tmp+rename、空表拒写。
- 官方 slug 集合：由混合目录生成并落 `codex_official_slugs.json`，可被分流读取；集合不含第三方 slug。
- `POST /backend-api/codex/responses`：`gpt-6.1-sol` 与 `codex-auto-review`（非 `gpt-` 前缀官方 slug）都走假官方上游；`qoder.DeepSeek-Flash` 走 fake 线路；未知 slug 返回 `404 unknown_model` 且不打任何上游；各断言一次且不互相串。
- 官方 slug 集合缺失时发 responses → 现场只读重算一次集合后再分流，不算到则不透传。
- 现有 `/v1/models`、`/v1/responses`、`/v1/codex/models` 行为不变（回归断言）。
- 日志断言：不含 `Authorization`、不含账号头原值、不含 token、不含请求体。

### 8.2 配置测试（临时 `CODEX_HOME`）

- 接管后顶层有 `openai_base_url`、有 `model_catalog_json`、**无 `model_provider`**；`[profiles.official]` 与用户注释仍在。
- 备份文件生成且与接管前内容逐字节一致；`auth.json` 字节不变。
- 还原后 `config.toml` 与接管前逐字节一致；混合目录备份可恢复。
- `config.toml` 写入后 `python3 tomllib` 可解析；混合目录 `json.Unmarshal` 可解析。
- 回归固定 4.3：移除 `model_provider` 后 responses 落 `/backend-api/codex/responses`（用临时 `CODEX_HOME` + 假后端或静态断言配置行）。

### 8.3 回归与验收

- `go test ./... -count=1`、`go vet ./...`、`gofmt -l`、`scripts/build.sh` 全绿。
- 预览实例（临时数据根 + 非生产端口）用 `curl` 带假 Authorization 打 `/backend-api/codex/responses`，确认官方 / 第三方 / 未知三类分流。
- 真实验收（用户重开 Codex 后）：`/model` 同屏出现官方推荐 + aihub 第三方；各选一个发消息成功；`codex --profile official` 仍能直连。设计阶段**不消耗官方额度**：带真实登录态的完整官方请求只在用户验收时产生，本规格不要求 agent 主动发。

## 9. 交付顺序（供实施计划展开，不在本规格里实现）

1. 写测试并实现 `codex_backend.go` 的混合目录生成 + 官方 slug 集合 + 4.3 配置落点回归。
2. 写测试并实现 `/backend-api/codex/responses` 分流（含 `resolveAggThirdParty` 抽取）。
3. 写测试并改 `client_takeover.go` / `assets/console.html` 的混合接管、状态与还原。
4. 删 `officialCatalogModels` 零调用残留常量，跑全量回归。
5. 构建 bin/aihub、PII 扫描、提交；给用户明确的「重开 Codex」与「必要时重启 aihub」命令。
6. 用户重开 Codex 验收；失败按回滚清单恢复备份。

## 10. 已核实但未改的事实（供实施者核对）

- `hub-go/client_takeover.go:123 takeOverCodex()`、`:273 restoreCodex()`、`:393 apiClientsTakeover()`、`:366 apiClientsRestore()`；`hub-go/main.go:128-131` 注册三个 `/api/clients/*`；`hub-go/assets/console.html:1402` / `:1419` / `:2591` 当前用 `codex_provider === 'aihub'` 判定已接管，需改为 `openai_base_url` 口径；`hub-go/client_takeover.go:444 apiClientsStatus()` 的 `m[1]` 无匹配会 panic，本期顺手修。
- `hub-go/proxy_agg.go:501 func resolveAggModel(slug string) (aggModel, bool)` 当前前三级匹配白名单 / `ccs-` / `<线路>.<模型>`，最后 `:533` 用 `isOfficialModelSlug(slug)` 把 `gpt-*` 解析成 `aggModel{Line:"official"}`；新链路只抽前三级为 `resolveAggThirdParty`，现有 `/v1/*` 的官方兜底保持不变。
- `hub-go/proxy_agg.go:324` 注册 `/v1/codex/models`；`:767` 调 `officialPassthrough`；`hub-go/proxy_official.go:23 const officialBackendURL = "https://chatgpt.com/backend-api/codex/responses"`；`hub-go/proxy_agg.go:201 var aggPrettyNames` 由 `aggDisplayName` 引用，不是可删残留。`hub-go/proxy_agg.go:729 func (h *Hub) aggDispatch(w http.ResponseWriter, r *http.Request, kind string)` 是第三方 responses 入口。
- `hub-go/codex_catalog.go:90 func writeCodexCatalogFile(h *Hub) (string, error)` 当前从 `aggSnapshot()` 生成 33 键模板条目、priority `i+1`、空表拒写、tmp+rename；A′ 复用其条目生成逻辑但输出改为「官方 + 第三方」。
- 当前线上 `aihub-console` 已于 `2026-11-09 08:54:46` 恢复；初版规格提交 `2bb2d51`，本修订在其后；工作区除本修订外 0 改动。
- magpie `codexMerged` / `codexListKey` / `codexcat.FitOwn` / `Keep` 的结论来自 GitHub raw 源码，仅作设计参考，不复制其代码；aihub 采用自己的实现与测试。第 0 步已证明本机客户端不走 magpie 主链路，故这些机制本期不实现。
