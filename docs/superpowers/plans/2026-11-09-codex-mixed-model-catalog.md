# Codex 混合模型目录（A′）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Codex 的同一个 `/model` 选择器里同时出现「本机 Codex 内置官方模型」与「aihub 聚合白名单里的第三方模型」，并让两类模型的 responses 请求都经 aihub 正确分流。

**Architecture:** aihub 从本机 `codex debug models --bundled` 取官方条目、从 `aggSnapshot()` 取第三方条目，合并写成 `~/.codex/aihub-model-catalog.json`（Codex 只在启动时读一次）；`~/.codex/config.toml` 顶层加 `openai_base_url = "http://127.0.0.1:8790/backend-api/codex"`、移除 `model_provider`、保留 `model_catalog_json`；聚合口新增 `POST /backend-api/codex/responses`，按「第三方 → 官方 slug 集合 → 404」三分支分流。

**Tech Stack:** Go 1.27（`/usr/local/go/bin/go`，不在 PATH）、`net/http` + `httptest`、`encoding/json`、现有 `backendClient`/`directClientFor`、`os/exec` 调用本机 Codex CLI。

## Global Constraints

- 唯一权威设计基准：`docs/superpowers/specs/2026-11-09-codex-mixed-model-catalog-design.md`（修订 A′，提交 `d20add0`）。冲突时以规格为准。
- Go 工具链不在 PATH：每条命令前 `export PATH=/usr/local/go/bin:$PATH`；所有 `go` 命令在 `hub-go/` 子目录执行（`go.mod` 在那里，在仓库根跑会报 `go: cannot find main module`）。
- 命令里的 `$REPO` 指项目根（本机为 `/home/<user>/opt/AI_test/hub-go`）；执行前先 `REPO=$(git rev-parse --show-toplevel)`。
- 不引入任何第三方依赖（`go.mod` 保持零直接依赖之外现状）。
- 新增远程 HTTP 调用必须走 `backendClient`/`directClientFor`；不得出现裸 `&http.Client{Timeout`（`TestNoBareHTTPClientOnRemotePaths` 会拦）。
- **不读、不改、不落盘 `~/.codex/auth.json`**；测试里用临时 home（`isolateUserHome(t)`），绝不碰真实家目录。
- 日志只记状态码/条目数/回退原因；绝不记 `Authorization`、`chatgpt-account-id` 原值、token、密钥、请求体。
- 混合目录上限：`4 << 20` 字节、200 条目；空表拒写（Codex 报 `must contain at least one model` 且不自愈）。
- 每个新函数必须有测试；先看到 RED 再实现 GREEN。
- 每个任务结束 `gofmt -l` 干净、`go vet ./...` 退出码 0，然后 commit。
- 不重启承载当前会话的 dsh；本会话模型链路是 dsh→:8790 agg→:8797 qoder，重启 qoder/agg 会断。
- 不擅自 `git push`；推送前按 `.tmp/pii-patterns.txt`（本地、不入库）逐项跑 PII 扫描，本计划正文不内联真实值。
- 全程中文注释与中文用户可见文案；代码标识符用英文。

## File Structure

| 文件 | 职责 | 新建/修改 |
|---|---|---|
| `hub-go/codex_official.go` | 从本机 Codex CLI 取官方条目；官方 slug 集合的内存/磁盘缓存 | 新建 |
| `hub-go/codex_official_test.go` | 上述的单元测试（假二进制、假 bundled JSON） | 新建 |
| `hub-go/codex_catalog.go` | 抽出 `codexThirdPartyEntries`；新增 `writeCodexMixedCatalog` | 修改 |
| `hub-go/codex_catalog_mixed_test.go` | 混合目录合并/上限/回退测试 | 新建 |
| `hub-go/codex_backend.go` | `/backend-api/codex/responses` 三分支分流 | 新建 |
| `hub-go/codex_backend_test.go` | 分流测试（假官方上游 + 假线路） | 新建 |
| `hub-go/proxy_agg.go` | 抽 `resolveAggThirdParty`；注册新路由 | 修改 |
| `hub-go/proxy_agg_thirdparty_test.go` | `resolveAggThirdParty` 与 `resolveAggModel` 行为等价性测试 | 新建 |
| `hub-go/client_takeover.go` | 接管/恢复改写为 `openai_base_url` 口径；状态接口修 panic | 修改 |
| `hub-go/client_takeover_test.go` | 临时 `CODEX_HOME` 下的接管/还原/状态测试 | 新建 |
| `hub-go/proxy_official.go` | 删零调用残留 `officialCatalogModels` | 修改 |
| `hub-go/assets/console.html` | 接管卡片文案与 4 处状态判定改 `openai_base_url` 口径 | 修改 |
| `hub-go/console_codex_takeover_test.go` | 前端静态断言 | 新建 |

---

### Task 1: 官方条目与官方 slug 集合

**Files:**
- Create: `hub-go/codex_official.go`
- Test: `hub-go/codex_official_test.go`

**Interfaces:**
- Consumes: `dataPath(elem ...string) string`（`hub-go/paths.go:120`）、`logHub(format string, a ...any)`（`hub-go/platform.go:51`）。
- Produces:
  - `func codexBinPath() string`
  - `var codexBundledJSON func() ([]byte, error)`
  - `func codexOfficialEntries() ([]map[string]any, error)`
  - `func codexOfficialSlugList(entries []map[string]any) []string`
  - `func saveCodexOfficialSlugs(slugs []string) error`
  - `func loadCodexOfficialSlugs() []string`
  - `func codexOfficialSlugSet() map[string]bool`
  - `const codexOfficialSlugsFile = "codex_official_slugs.json"`

- [ ] **Step 1: 写失败测试**

创建 `hub-go/codex_official_test.go`：

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// bundledSample 是本机 `codex debug models --bundled` 的真实形态（截取 3 条）。
const bundledSample = `{"models":[
 {"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","visibility":"list","priority":1,"description":"d"},
 {"slug":"codex-auto-review","display_name":"Codex Auto Review","visibility":"hide","priority":43,"description":"d"},
 {"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","priority":5,"description":"d"}
]}`

// isolateOfficialDataRoot 把数据根指到临时目录（官方 slug 集合落在这里）。
func isolateOfficialDataRoot(t *testing.T) string {
	t.Helper()
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })
	return dir
}

// withBundledJSON 替换内置目录来源，避免测试依赖本机是否装了 Codex。
func withBundledJSON(t *testing.T, raw string) {
	t.Helper()
	old := codexBundledJSON
	codexBundledJSON = func() ([]byte, error) { return []byte(raw), nil }
	t.Cleanup(func() { codexBundledJSON = old })
}

func TestCodexOfficialEntriesSortedListFirst(t *testing.T) {
	withBundledJSON(t, bundledSample)
	entries, err := codexOfficialEntries()
	if err != nil {
		t.Fatalf("取官方条目失败：%v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d，应为 3", len(entries))
	}
	// visibility=list 必须排在 hide 之前
	if entries[len(entries)-1]["slug"] != "codex-auto-review" {
		t.Errorf("hide 条目应排最后，实际最后一条 = %v", entries[len(entries)-1]["slug"])
	}
	// 官方字段不许被改写
	if entries[0]["display_name"] == nil || entries[0]["description"] == nil {
		t.Error("官方条目的 display_name/description 丢了")
	}
}

func TestCodexOfficialEntriesRejectsEmpty(t *testing.T) {
	withBundledJSON(t, `{"models":[]}`)
	if _, err := codexOfficialEntries(); err == nil {
		t.Fatal("空目录必须报错，否则会写出空 catalog")
	}
}

func TestCodexOfficialSlugsRoundTrip(t *testing.T) {
	dir := isolateOfficialDataRoot(t)
	slugs := []string{"gpt-6.1-sol", "codex-auto-review"}
	if err := saveCodexOfficialSlugs(slugs); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	path := filepath.Join(dir, codexOfficialSlugsFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("文件不存在：%v", err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("权限 = %v，应为 0600（含官方模型名单，不该世界可读）", st.Mode().Perm())
	}
	got := loadCodexOfficialSlugs()
	if len(got) != 2 || got[0] != "gpt-6.1-sol" {
		t.Fatalf("读回 = %v", got)
	}
	// 内存缓存必须同步生效
	set := codexOfficialSlugSet()
	if !set["codex-auto-review"] {
		t.Error("内存缓存里没有 codex-auto-review")
	}
}

func TestCodexOfficialSlugSetFallsBackToBundle(t *testing.T) {
	isolateOfficialDataRoot(t) // 磁盘上没有任何 slug 文件
	officialSlugMu.Lock()
	officialSlugCache = nil
	officialSlugMu.Unlock()
	withBundledJSON(t, bundledSample)

	set := codexOfficialSlugSet()
	if !set["gpt-6.1-sol"] || !set["codex-auto-review"] {
		t.Fatalf("回退重算没拿到官方 slug：%v", set)
	}
	// 回退重算不应写盘（只读路径）
	if _, err := os.Stat(codexOfficialSlugsPath()); err == nil {
		t.Error("只读重算不该落盘")
	}
}

func TestCodexOfficialSlugListSkipsEmptySlug(t *testing.T) {
	got := codexOfficialSlugList([]map[string]any{{"slug": "a"}, {"slug": ""}, {}})
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("slug 列表 = %v", got)
	}
	_ = json.Valid
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestCodexOfficial' -count=1 .
```

预期输出（关键行）：

```
# aihub [aihub.test]
./codex_official_test.go:60:16: undefined: codexOfficialEntries
./codex_official_test.go:78:16: undefined: codexOfficialEntries
./codex_official_test.go:87:9: undefined: saveCodexOfficialSlugs
FAIL	aihub [build failed]
FAIL
```

- [ ] **Step 3: 实现**

创建 `hub-go/codex_official.go`：

```go
package main

// Codex 官方模型目录（A′ 方案，2026-11-09）。
//
// 背景：实测本机 Codex 0.167 的模型选择器目录只来自 model_catalog_json 或
// 内置 bundled 目录，**不会**去请求 openai_base_url 的 /models（见
// docs/superpowers/specs/2026-11-09-codex-mixed-model-catalog-design.md 第 1.1 节）。
// 所以官方条目改从 `codex debug models --bundled` 取：只读、不联网、
// 不需要登录态、不消耗官方额度。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const codexOfficialSlugsFile = "codex_official_slugs.json"

// codexBinPath 本机 Codex CLI 路径；测试用 AIHUB_CODEX_BIN 注入假二进制。
func codexBinPath() string {
	if p := strings.TrimSpace(os.Getenv("AIHUB_CODEX_BIN")); p != "" {
		return p
	}
	return "/usr/lib/chatgpt/resources/codex"
}

// codexBundledJSON 取内置目录原文。做成变量便于测试替换。
var codexBundledJSON = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, codexBinPath(), "debug", "models", "--bundled").Output()
	if err != nil {
		return nil, fmt.Errorf("运行 codex debug models --bundled 失败：%w", err)
	}
	return out, nil
}

// codexOfficialEntries 解析内置目录；visibility=list 在前、hide 在后。
func codexOfficialEntries() ([]map[string]any, error) {
	raw, err := codexBundledJSON()
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析内置目录失败：%w", err)
	}
	out := make([]map[string]any, 0, len(doc.Models))
	for _, e := range doc.Models {
		if s, _ := e["slug"].(string); s != "" {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("内置目录为空或没有可用 slug")
	}
	sort.SliceStable(out, func(i, j int) bool {
		return codexVisibilityRank(out[i]) < codexVisibilityRank(out[j])
	})
	return out, nil
}

func codexVisibilityRank(e map[string]any) int {
	if v, _ := e["visibility"].(string); v == "hide" {
		return 1
	}
	return 0
}

// codexOfficialSlugList 取条目里的 slug 列表（保持顺序）。
func codexOfficialSlugList(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if s, _ := e["slug"].(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// codexOfficialSlugsPath 官方 slug 集合落盘位置（数据根，不进家目录）。
func codexOfficialSlugsPath() string { return dataPath(codexOfficialSlugsFile) }

var (
	officialSlugMu    sync.RWMutex
	officialSlugCache map[string]bool
)

// saveCodexOfficialSlugs 落盘并刷新内存缓存。0600：内容是官方模型名单。
func saveCodexOfficialSlugs(slugs []string) error {
	b, err := json.MarshalIndent(map[string]any{"slugs": slugs}, "", " ")
	if err != nil {
		return err
	}
	path := codexOfficialSlugsPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	setOfficialSlugCache(slugs)
	return nil
}

// loadCodexOfficialSlugs 读盘；文件缺失/损坏返回 nil（不报错）。
func loadCodexOfficialSlugs() []string {
	b, err := os.ReadFile(codexOfficialSlugsPath())
	if err != nil {
		return nil
	}
	var doc struct {
		Slugs []string `json:"slugs"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return doc.Slugs
}

func setOfficialSlugCache(slugs []string) {
	m := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		if s != "" {
			m[s] = true
		}
	}
	officialSlugMu.Lock()
	officialSlugCache = m
	officialSlugMu.Unlock()
}

// codexOfficialSlugSet 取官方 slug 集合：内存 → 磁盘 → 现场只读重算。
// 只读重算不落盘（落盘只发生在生成混合目录时）。
func codexOfficialSlugSet() map[string]bool {
	officialSlugMu.RLock()
	cached := officialSlugCache
	officialSlugMu.RUnlock()
	if len(cached) > 0 {
		return cached
	}
	if slugs := loadCodexOfficialSlugs(); len(slugs) > 0 {
		setOfficialSlugCache(slugs)
		officialSlugMu.RLock()
		defer officialSlugMu.RUnlock()
		return officialSlugCache
	}
	entries, err := codexOfficialEntries()
	if err != nil {
		logHub("官方 slug 集合缺失且重算失败：%v", err)
		return nil
	}
	setOfficialSlugCache(codexOfficialSlugList(entries))
	officialSlugMu.RLock()
	defer officialSlugMu.RUnlock()
	return officialSlugCache
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestCodexOfficial' -count=1 -v .
```

预期输出：

```
=== RUN   TestCodexOfficialEntriesSortedListFirst
--- PASS: TestCodexOfficialEntriesSortedListFirst (0.00s)
=== RUN   TestCodexOfficialEntriesRejectsEmpty
--- PASS: TestCodexOfficialEntriesRejectsEmpty (0.00s)
=== RUN   TestCodexOfficialSlugsRoundTrip
--- PASS: TestCodexOfficialSlugsRoundTrip (0.00s)
=== RUN   TestCodexOfficialSlugSetFallsBackToBundle
--- PASS: TestCodexOfficialSlugSetFallsBackToBundle (0.00s)
=== RUN   TestCodexOfficialSlugListSkipsEmptySlug
--- PASS: TestCodexOfficialSlugListSkipsEmptySlug (0.00s)
PASS
ok  	aihub	0.02s
```

- [ ] **Step 5: 格式与静态检查**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
gofmt -l codex_official.go codex_official_test.go
go vet ./...
```

预期输出：`gofmt` 无输出（表示干净），`go vet` 无输出、退出码 0。

- [ ] **Step 6: 提交**

```bash
cd $REPO
git add hub-go/codex_official.go hub-go/codex_official_test.go
git commit -m "feat(codex): 从本机 Codex 内置目录取官方模型与官方 slug 集合"
```

---

### Task 2: 混合目录生成

**Files:**
- Modify: `hub-go/codex_catalog.go:84-190`（抽 `codexThirdPartyEntries`）、追加 `writeCodexMixedCatalog`
- Test: `hub-go/codex_catalog_mixed_test.go`

**Interfaces:**
- Consumes: Task 1 的 `codexOfficialEntries()`、`codexOfficialSlugList()`、`saveCodexOfficialSlugs()`；现有 `catalogTemplate()`、`cloneJSONMap()`、`aggSnapshot()`、`aggOpenAIDisplayName(h *Hub, am aggModel) string`、`backupFile(path string) (string, error)`、`homeJoin(elem ...string) (string, error)`、`codexCatalogPath`。
- Produces:
  - `func codexThirdPartyEntries(h *Hub) ([]map[string]any, error)`
  - `func writeCodexMixedCatalog(h *Hub) (string, error)`
  - `const codexMixedMaxBytes = 4 << 20`、`const codexMixedMaxEntries = 200`

- [ ] **Step 1: 写失败测试**

创建 `hub-go/codex_catalog_mixed_test.go`：

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupMixedEnv 准备临时 home + 临时数据根 + 固定 aggList。
func setupMixedEnv(t *testing.T) string {
	t.Helper()
	home := isolateUserHome(t)
	isolateOfficialDataRoot(t)
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{
		{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "DeepSeek V4 Flash（国内）"},
		{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash", Display: "DeepSeek Flash（Qoder）"},
	}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })
	return home
}

func readMixedCatalog(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("目录不是合法 JSON：%v", err)
	}
	return doc.Models
}

func TestWriteCodexMixedCatalogMergesOfficialAndThirdParty(t *testing.T) {
	home := setupMixedEnv(t)
	withBundledJSON(t, bundledSample)

	path, err := writeCodexMixedCatalog(&Hub{})
	if err != nil {
		t.Fatalf("写混合目录失败：%v", err)
	}
	if want := filepath.Join(home, ".codex", codexCatalogPath); path != want {
		t.Fatalf("落盘路径 = %q，应为 %q", path, want)
	}
	models := readMixedCatalog(t, path)
	if len(models) != 5 { // 官方 3 + 第三方 2
		t.Fatalf("条目数 = %d，应为 5", len(models))
	}
	// 官方在前、第三方在后
	if models[0]["slug"] != "gpt-6.1-sol" && models[0]["slug"] != "gpt-5.5" {
		t.Errorf("第一条应是官方模型，实际 %v", models[0]["slug"])
	}
	last := models[len(models)-1]
	if !strings.HasPrefix(last["slug"].(string), "qoder.") && !strings.HasPrefix(last["slug"].(string), "dom.") {
		t.Errorf("最后一条应是第三方模型，实际 %v", last["slug"])
	}
	// priority 连续编号
	for i, m := range models {
		if p, _ := m["priority"].(float64); int(p) != i+1 {
			t.Errorf("第 %d 条 priority = %v，应为 %d", i, m["priority"], i+1)
		}
	}
	// 官方条目字段不被改写
	if models[0]["description"] == nil {
		t.Error("官方条目丢了 description")
	}
	// 官方 slug 集合已落盘
	if slugs := loadCodexOfficialSlugs(); len(slugs) != 3 {
		t.Errorf("官方 slug 集合 = %v，应为 3 条", slugs)
	}
}

func TestWriteCodexMixedCatalogFallsBackWhenBundledBroken(t *testing.T) {
	home := setupMixedEnv(t)
	// bundled 不可用，且没有 models_cache.json → 只写第三方
	old := codexBundledJSON
	codexBundledJSON = func() ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { codexBundledJSON = old })

	path, err := writeCodexMixedCatalog(&Hub{})
	if err != nil {
		t.Fatalf("只写第三方也应成功：%v", err)
	}
	models := readMixedCatalog(t, filepath.Join(home, ".codex", codexCatalogPath))
	if len(models) != 2 {
		t.Fatalf("条目数 = %d，应为 2（只有第三方）", len(models))
	}
}

func TestWriteCodexMixedCatalogUsesModelsCacheFallback(t *testing.T) {
	home := setupMixedEnv(t)
	old := codexBundledJSON
	codexBundledJSON = func() ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { codexBundledJSON = old })
	// 写一份 models_cache.json 作为兜底来源
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := `{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","description":"d"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}

	path, err := writeCodexMixedCatalog(&Hub{})
	if err != nil {
		t.Fatalf("回退缓存也应成功：%v", err)
	}
	models := readMixedCatalog(t, path)
	if len(models) != 3 { // 缓存 1 + 第三方 2
		t.Fatalf("条目数 = %d，应为 3", len(models))
	}
}

func TestWriteCodexMixedCatalogRefusesEmpty(t *testing.T) {
	home := setupMixedEnv(t)
	old := codexBundledJSON
	codexBundledJSON = func() ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { codexBundledJSON = old })
	aggMu.Lock()
	saved := aggList
	aggList = nil
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	// 预置一个非空文件，确认空表时不会被覆盖成 {"models": []}
	path := filepath.Join(home, ".codex", codexCatalogPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := `{"models":[{"slug":"keep-me","display_name":"Keep","description":"d"}]}`
	if err := os.WriteFile(path, []byte(keep), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeCodexMixedCatalog(&Hub{}); err == nil {
		t.Fatal("空表必须报错，否则 Codex 打不开")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "keep-me") {
		t.Error("空表时不该覆盖已有目录文件")
	}
}

func TestWriteCodexMixedCatalogCapsEntries(t *testing.T) {
	setupMixedEnv(t)
	// 造 250 条官方条目
	var sb strings.Builder
	sb.WriteString(`{"models":[`)
	for i := 0; i < 250; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"slug":"gpt-x-`)
		sb.WriteString(strings.Repeat("a", 1))
		sb.WriteString(`-`)
		sb.WriteString(itoaForTest(i))
		sb.WriteString(`","display_name":"X","visibility":"list","description":"d"}`)
	}
	sb.WriteString(`]}`)
	withBundledJSON(t, sb.String())

	path, err := writeCodexMixedCatalog(&Hub{})
	if err != nil {
		t.Fatalf("超限应截断而非报错：%v", err)
	}
	models := readMixedCatalog(t, path)
	if len(models) != codexMixedMaxEntries {
		t.Fatalf("条目数 = %d，应为 %d", len(models), codexMixedMaxEntries)
	}
}

// itoaForTest 避免为了造数据引入 strconv 依赖噪音。
func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestWriteCodexMixedCatalog' -count=1 .
```

预期输出：

```
# aihub [aihub.test]
./codex_catalog_mixed_test.go:60:14: undefined: writeCodexMixedCatalog
./codex_catalog_mixed_test.go:96:14: undefined: writeCodexMixedCatalog
./codex_catalog_mixed_test.go:122:14: undefined: writeCodexMixedCatalog
./codex_catalog_mixed_test.go:139:18: undefined: writeCodexMixedCatalog
./codex_catalog_mixed_test.go:170:14: undefined: writeCodexMixedCatalog
FAIL	aihub [build failed]
FAIL
```

- [ ] **Step 3: 抽出第三方条目生成函数（行为保持）**

把 `hub-go/codex_catalog.go` 里 `writeCodexCatalogFile` 的条目生成段抽成独立函数，`writeCodexCatalogFile` 改为调用它。替换 `hub-go/codex_catalog.go:90-153`（从 `func writeCodexCatalogFile` 到 `entries = append(entries, e)` 结束的 `}`）为下面两段：

```go
// codexThirdPartyEntries 把聚合白名单转成 Codex 目录条目（33+ 键模板克隆）。
// 抽出来是为了让「只写第三方」的旧路径与「官方 + 第三方」的混合路径共用同一套
// display_name / 上下文窗口 / 分组排序逻辑，避免两处漂移。
func codexThirdPartyEntries(h *Hub) ([]map[string]any, error) {
	tpl := catalogTemplate()
	if tpl == nil {
		return nil, fmt.Errorf("内置目录模板缺失，桌面版目录跳过（CLI 不受影响）")
	}
	list := append([]aggModel{}, aggSnapshot()...)
	sort.Slice(list, func(i, j int) bool {
		ri, rj := catalogGroupRank(list[i].Line), catalogGroupRank(list[j].Line)
		if ri != rj {
			return ri < rj
		}
		return list[i].Slug < list[j].Slug
	})
	type item struct {
		slug, display, desc string
	}
	items := make([]item, 0, len(list))
	for _, am := range list {
		display := am.Display
		if h != nil {
			display = aggOpenAIDisplayName(h, am)
		}
		items = append(items, item{slug: am.Slug, display: display,
			desc: display + " · AIHub 统一网关"})
	}
	declareCtx := 0
	if h != nil {
		declareCtx = h.contextWindow()
	}
	entries := make([]map[string]any, 0, len(items))
	for i, it := range items {
		e, err := cloneJSONMap(tpl[i%len(tpl)])
		if err != nil {
			e = map[string]any{}
		}
		e["slug"] = it.slug
		e["display_name"] = it.display
		e["description"] = it.desc
		e["priority"] = i + 1
		if declareCtx > 0 {
			e["context_window"] = declareCtx
			e["max_context_window"] = declareCtx
			e["auto_compact_token_limit"] = declareCtx * 9 / 10
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// writeCodexCatalogFile 只写第三方模型（旧行为，保留给「官方目录不可用」时的手工路径）。
func writeCodexCatalogFile(h *Hub) (string, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	entries, err := codexThirdPartyEntries(h)
	if err != nil {
		return "", err
	}
	path, err := homeJoin(".codex", codexCatalogPath)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		logHub("⚠ 桌面版目录跳过：白名单 0 个模型，保留现有文件不覆盖（%s）", path)
		return "", fmt.Errorf("白名单为空，保留现有目录文件：%s", path)
	}
	out, err := json.MarshalIndent(map[string]any{"models": entries}, "", " ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	_, statErr := os.Stat(path)
	overwriting := statErr == nil
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if overwriting {
		logHub("桌面版目录已刷新：%d 个模型 → %s", len(entries), path)
	}
	return path, nil
}
```

- [ ] **Step 4: 跑既有目录测试确认重构没破坏行为**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestCodexCatalog|TestWBCatalogRefreshTriggersCodexCatalog' -count=1 -v .
```

预期输出：`TestCodexCatalogDisplayNameCarriesRatio`、`TestCodexCatalogFileLocation`、`TestCodexCatalogSkipsEmptyWhitelist`、`TestWBCatalogRefreshTriggersCodexCatalog` 全部 `--- PASS`，末尾 `ok aihub`。

- [ ] **Step 5: 实现混合目录**

在 `hub-go/codex_catalog.go` 末尾追加：

```go
// ---------- A′ 混合目录（2026-11-09） ----------

const (
	// 混合目录上限：实测本机 19 条约 1.07 MB，4 MiB 留足余量。
	codexMixedMaxBytes = 4 << 20
	// Codex 选择器可读的条目上限；超出按「官方 list → 官方 hide → 第三方」截断。
	codexMixedMaxEntries = 200
)

// codexOfficialEntriesFromCache 回退来源：~/.codex/models_cache.json 的 models。
// 只读，不打印 identity/etag（那份文件含账号标识）。
func codexOfficialEntriesFromCache() []map[string]any {
	path, err := homeJoin(".codex", "models_cache.json")
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(doc.Models))
	for _, e := range doc.Models {
		if s, _ := e["slug"].(string); s != "" {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return codexVisibilityRank(out[i]) < codexVisibilityRank(out[j])
	})
	return out
}

// writeCodexMixedCatalog 写「官方 + 第三方」混合目录（A′ 方案）。
//
// 顺序：官方 visibility=list → 官方 hide → 第三方；priority 连续编号。
// 官方条目原样保留（本地文件不走 HTTP 合并，无需删 availability_nux/upgrade）。
func writeCodexMixedCatalog(h *Hub) (string, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()

	official, officialErr := codexOfficialEntries()
	if officialErr != nil {
		if cached := codexOfficialEntriesFromCache(); len(cached) > 0 {
			official = cached
			logHub("官方目录回退 models_cache.json：%v", officialErr)
		} else {
			logHub("官方目录不可用，本次只写第三方：%v", officialErr)
		}
	}
	third, err := codexThirdPartyEntries(h)
	if err != nil {
		return "", err
	}
	entries := make([]map[string]any, 0, len(official)+len(third))
	entries = append(entries, official...)
	entries = append(entries, third...)
	if len(entries) > codexMixedMaxEntries {
		logHub("混合目录条目 %d 超上限 %d，按「官方 → 第三方」截断", len(entries), codexMixedMaxEntries)
		entries = entries[:codexMixedMaxEntries]
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("混合目录为空（官方与第三方都没有模型），保留现有文件不覆盖")
	}
	for i, e := range entries {
		e["priority"] = i + 1
	}
	out, err := json.MarshalIndent(map[string]any{"models": entries}, "", " ")
	if err != nil {
		return "", err
	}
	if len(out) > codexMixedMaxBytes {
		return "", fmt.Errorf("混合目录 %d 字节超过上限 %d", len(out), codexMixedMaxBytes)
	}
	var check struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &check); err != nil {
		return "", fmt.Errorf("混合目录自检失败：%w", err)
	}
	path, err := homeJoin(".codex", codexCatalogPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		if _, berr := backupFile(path); berr != nil {
			logHub("混合目录备份失败：%v", berr)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if len(official) > 0 {
		if err := saveCodexOfficialSlugs(codexOfficialSlugList(official)); err != nil {
			logHub("官方 slug 集合落盘失败：%v", err)
		}
	}
	logHub("混合目录已刷新：官方 %d + 第三方 %d = %d 个模型 → %s",
		len(official), len(third), len(entries), path)
	return path, nil
}
```

- [ ] **Step 6: 跑测试确认通过**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestWriteCodexMixedCatalog|TestCodexCatalog' -count=1 -v .
```

预期输出：5 条 `TestWriteCodexMixedCatalog*` 与 3 条 `TestCodexCatalog*` 全部 `--- PASS`，末尾 `ok aihub`。

- [ ] **Step 7: 格式、vet、提交**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
gofmt -l codex_catalog.go codex_catalog_mixed_test.go
go vet ./...
cd $REPO
git add hub-go/codex_catalog.go hub-go/codex_catalog_mixed_test.go
git commit -m "feat(codex): 混合目录 = 本机官方条目 + 聚合第三方条目"
```

预期：`gofmt`/`vet` 无输出；commit 输出 `[main <sha>] feat(codex): ...`。

---

### Task 3: `/backend-api/codex/responses` 三分支分流

**Files:**
- Modify: `hub-go/proxy_agg.go:501-538`（抽 `resolveAggThirdParty`）、`hub-go/proxy_agg.go:329-331`（注册路由）
- Create: `hub-go/codex_backend.go`
- Test: `hub-go/proxy_agg_thirdparty_test.go`、`hub-go/codex_backend_test.go`

**Interfaces:**
- Consumes: Task 1 的 `codexOfficialSlugSet() map[string]bool`；现有 `aggSnapshot()`、`resolveDesktopRole(slug string) string`、`aggCCSKeys`、`aggSelectableLine(line string) bool`、`aggLineCN`、`isOfficialModelSlug(slug string) bool`、`requestedModelSlug(raw []byte) string`、`aggDispatch(w,r,kind)`、`officialPassthrough(w,r,nb)`、`writeJSON`、`recordUsage(usageRec{...})`。
- Produces:
  - `func resolveAggThirdParty(slug string) (aggModel, bool)`
  - `func (h *Hub) codexBackendResponses(w http.ResponseWriter, r *http.Request)`
  - `const codexUnknownModelMsg = "未知模型（既不在聚合白名单/线路前缀里，也不在本机 Codex 官方目录里）"`

- [ ] **Step 1: 写 `resolveAggThirdParty` 的等价性测试**

创建 `hub-go/proxy_agg_thirdparty_test.go`：

```go
package main

import "testing"

// resolveAggThirdParty 必须与 resolveAggModel 在「第三方」范围内完全等价：
// 官方 gpt-* 由 resolveAggModel 兜底，但 resolveAggThirdParty 必须拒绝。
func TestResolveAggThirdPartyExcludesOfficial(t *testing.T) {
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "D"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	cases := []struct {
		slug string
		want bool
	}{
		{"dom.deepseek-v4-flash", true},   // 白名单精确命中
		{"qoder.DeepSeek-Flash", true},    // <线路>.<模型> 动态形式
		{"intl.some-new-model", true},     // 线路已注册即可
		{"gpt-6.1-sol", false},            // 官方前缀：第三方解析必须拒绝
		{"codex-auto-review", false},      // 非 gpt- 官方 slug 也不是第三方
		{"ccs-dom/deepseek-v4-flash", true}, // Hub Switch 时代写法
		{"totally-unknown", false},
	}
	for _, c := range cases {
		if _, ok := resolveAggThirdParty(c.slug); ok != c.want {
			t.Errorf("resolveAggThirdParty(%q) = %v，应为 %v", c.slug, ok, c.want)
		}
	}
	// 官方前缀在完整 resolveAggModel 里仍然命中（现有 /v1/* 行为不变）
	if am, ok := resolveAggModel("gpt-6.1-sol"); !ok || am.Line != "official" {
		t.Errorf("resolveAggModel(gpt-6.1-sol) = %+v, %v；官方兜底不能丢", am, ok)
	}
}

// 重构后 resolveAggModel 对第三方 slug 的解析结果必须逐字段不变。
func TestResolveAggModelUnchangedForThirdParty(t *testing.T) {
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "D"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	for _, slug := range []string{"dom.deepseek-v4-flash", "qoder.DeepSeek-Flash", "ccs-dom/deepseek-v4-flash"} {
		a, aok := resolveAggThirdParty(slug)
		b, bok := resolveAggModel(slug)
		if aok != bok || a != b {
			t.Errorf("%q：第三方解析 %+v/%v ≠ 完整解析 %+v/%v", slug, a, aok, b, bok)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestResolveAggThirdParty|TestResolveAggModelUnchanged' -count=1 .
```

预期输出：

```
# aihub [aihub.test]
./proxy_agg_thirdparty_test.go:26:21: undefined: resolveAggThirdParty
./proxy_agg_thirdparty_test.go:27:21: undefined: resolveAggThirdParty
...
FAIL	aihub [build failed]
```

- [ ] **Step 3: 抽出 `resolveAggThirdParty`**

把 `hub-go/proxy_agg.go:501-538` 的 `resolveAggModel` 整体替换为：

```go
// resolveAggThirdParty 只解析「第三方」模型：白名单 / ccs- / <线路>.<模型>。
// A′ 的 Codex 原生路径用它判定「是不是第三方」，从而不会把官方 slug 当成线路模型。
func resolveAggThirdParty(slug string) (aggModel, bool) {
	for _, am := range aggSnapshot() {
		if am.Slug == slug {
			return am, true
		}
	}
	// Claude Desktop 安全角色名（claude-sonnet-5 等，含日期后缀）→ 聚合 slug
	if s := resolveDesktopRole(slug); s != "" {
		if am, ok := resolveAggModel(s); ok {
			return am, true
		}
	}
	// ccs- 形式（含 claude 桌面版的 key--model 写法）
	if rest, ok := strings.CutPrefix(slug, "ccs-"); ok {
		var key, model string
		if i := strings.Index(rest, "/"); i >= 0 {
			key, model = rest[:i], rest[i+1:]
		} else if i := strings.Index(rest, "--"); i >= 0 {
			key, model = rest[:i], rest[i+2:]
		}
		if line, ok := aggCCSKeys[key]; ok && model != "" {
			return aggModel{Line: line, Slug: slug, Backend: model,
				Display: model + "（" + aggLineCN[line] + "）"}, true
		}
		return aggModel{}, false
	}
	// <线路>.<模型> 动态形式
	if line, model, found := strings.Cut(slug, "."); found && aggSelectableLine(line) && model != "" {
		return aggModel{Line: line, Slug: slug, Backend: model,
			Display: model + "（" + aggLineCN[line] + "）"}, true
	}
	return aggModel{}, false
}

// resolveAggModel 统一网关的模型名解析（2026-10-13 方案 C 升级）。
// 前三级与 resolveAggThirdParty 完全相同；最后一级把官方 gpt-* 解析成 official。
func resolveAggModel(slug string) (aggModel, bool) {
	if am, ok := resolveAggThirdParty(slug); ok {
		return am, true
	}
	// 官方 ChatGPT 模型（gpt-*）：透传官方 API（2026-10-13 共存方案，见 proxy_official.go）。
	if isOfficialModelSlug(slug) {
		return aggModel{Line: "official", Slug: slug, Backend: slug,
			Display: slug + "（官方）"}, true
	}
	return aggModel{}, false
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestResolveAggThirdParty|TestResolveAggModelUnchanged' -count=1 -v .
```

预期输出：两条 `--- PASS`，末尾 `ok aihub`。

- [ ] **Step 5: 写分流测试（先失败）**

创建 `hub-go/codex_backend_test.go`：

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// codexBackendProbe 记录假上游收到的请求，供分流断言。
type codexBackendProbe struct {
	officialHits int
	officialBody string
	lineHits     int
	lineModel    string
}

// setupCodexBackend 把官方透传地址与线路转发都指向本地假上游。
// 返回探针与清理函数；不触碰真实网络、不消耗官方额度。
func setupCodexBackend(t *testing.T) *codexBackendProbe {
	t.Helper()
	probe := &codexBackendProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		probe.officialHits++
		probe.officialBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_probe","object":"response","output":[]}`))
	}))
	t.Cleanup(srv.Close)

	oldURL := officialBackendURLVar
	officialBackendURLVar = srv.URL
	t.Cleanup(func() { officialBackendURLVar = oldURL })

	// 官方 slug 集合固定，避免测试去跑本机 Codex CLI
	officialSlugMu.Lock()
	oldCache := officialSlugCache
	officialSlugCache = map[string]bool{"gpt-6.1-sol": true, "codex-auto-review": true}
	officialSlugMu.Unlock()
	t.Cleanup(func() {
		officialSlugMu.Lock()
		officialSlugCache = oldCache
		officialSlugMu.Unlock()
	})

	// 第三方线路：注入一个假适配器，直接回固定 JSON
	oldAdapters := lineAdapters
	lineAdapters = map[string]func(*Hub) lineAdapter{
		"dom": func(h *Hub) lineAdapter { return fakeCodexLine{h: h, probe: probe} },
	}
	t.Cleanup(func() { lineAdapters = oldAdapters })
	return probe
}

// fakeCodexLine 只实现 lineAdapter 里分流用得到的部分。
type fakeCodexLine struct {
	h     *Hub
	probe *codexBackendProbe
}

func (a fakeCodexLine) Key() string          { return "dom" }
func (a fakeCodexLine) Running() bool        { return true }
func (a fakeCodexLine) Start() (bool, string) { return true, "ok" }
func (a fakeCodexLine) Stop() (bool, string)  { return true, "ok" }
func (a fakeCodexLine) Chat(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	a.probe.lineHits++
	a.probe.lineModel = string(b)
	writeJSON(w, http.StatusOK, map[string]any{"object": "chat.completion", "choices": []any{}})
}
func (a fakeCodexLine) NativeResponses() http.HandlerFunc { return nil }
func (a fakeCodexLine) Models() []string                  { return nil }

func postCodexBackend(t *testing.T, h *Hub, body string, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.codexBackendResponses(rec, req)
	return rec
}

func TestCodexBackendRoutesOfficialSlug(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"gpt-6.1-sol","stream":false}`, "Bearer probe-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，应为 200；body=%s", rec.Code, rec.Body.String())
	}
	if probe.officialHits != 1 {
		t.Errorf("官方上游命中 = %d，应为 1", probe.officialHits)
	}
	if probe.lineHits != 0 {
		t.Errorf("不该打到线路，实际 %d 次", probe.lineHits)
	}
	// store=false 由 officialFixBody 补上
	var m map[string]any
	if err := json.Unmarshal([]byte(probe.officialBody), &m); err != nil {
		t.Fatalf("上游 body 不是 JSON：%v", err)
	}
	if v, ok := m["store"].(bool); !ok || v {
		t.Errorf("store 应为 false，实际 %v", m["store"])
	}
}

func TestCodexBackendRoutesNonGptOfficialSlug(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"codex-auto-review"}`, "Bearer probe-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("非 gpt- 前缀的官方 slug 也要走官方，状态 = %d", rec.Code)
	}
	if probe.officialHits != 1 || probe.lineHits != 0 {
		t.Errorf("official=%d line=%d，应 official=1 line=0", probe.officialHits, probe.lineHits)
	}
}

func TestCodexBackendRoutesThirdParty(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"dom.deepseek-v4-flash","stream":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("第三方应走线路，状态 = %d；body=%s", rec.Code, rec.Body.String())
	}
	if probe.lineHits != 1 {
		t.Errorf("线路命中 = %d，应为 1", probe.lineHits)
	}
	if probe.officialHits != 0 {
		t.Errorf("第三方不该打到官方，实际 %d 次", probe.officialHits)
	}
}

func TestCodexBackendUnknownModel404(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"totally-unknown"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知模型状态 = %d，应为 404；body=%s", rec.Code, rec.Body.String())
	}
	if probe.officialHits != 0 || probe.lineHits != 0 {
		t.Errorf("未知模型不许打任何上游：official=%d line=%d", probe.officialHits, probe.lineHits)
	}
	if !strings.Contains(rec.Body.String(), "unknown_model") {
		t.Errorf("错误体应含 unknown_model，实际 %s", rec.Body.String())
	}
}

func TestCodexBackendMissingModel400(t *testing.T) {
	setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"stream":true}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 model 状态 = %d，应为 400", rec.Code)
	}
}
```

- [ ] **Step 6: 跑测试确认失败**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestCodexBackend' -count=1 .
```

预期输出：

```
# aihub [aihub.test]
./codex_backend_test.go:31:2: undefined: officialBackendURLVar
./codex_backend_test.go:64:18: undefined: codexBackendResponses
./codex_backend_test.go:66:18: undefined: codexBackendResponses
...
FAIL	aihub [build failed]
```

- [ ] **Step 7: 把官方后端地址改成可注入变量**

修改 `hub-go/proxy_official.go:23`：

```go
// officialBackendURLVar 官方 responses 后端地址；测试用 httptest 覆盖它。
var officialBackendURLVar = officialBackendURL
```

并把 `hub-go/proxy_official.go:57` 的 `officialBackendURL` 改为 `officialBackendURLVar`：

```go
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, officialBackendURLVar, bytes.NewReader(nb))
```

- [ ] **Step 8: 实现分流 handler**

创建 `hub-go/codex_backend.go`：

```go
package main

// Codex 原生 responses 路径（A′ 方案，2026-11-09）。
//
// 背景：config.toml 顶层只留 openai_base_url 指向
// http://127.0.0.1:8790/backend-api/codex 时，Codex 的 /responses 会落到这里
// （实测 responses_hits=13、models_hits=0，见规格第 1.1/1.2 节）。
// 本文件按 model 字段三分支：第三方 → aggDispatch；官方 slug → officialPassthrough；
// 都不命中 → 404 unknown_model（不猜测路由，不把未知模型悄悄发给官方）。

import (
	"bytes"
	"io"
	"net/http"
)

const codexUnknownModelMsg = "未知模型（既不在聚合白名单/线路前缀里，也不在本机 Codex 官方目录里）"

// codexBackendResponses 处理 POST /backend-api/codex/responses。
func (h *Hub) codexBackendResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "read body: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	slug := requestedModelSlug(body)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体缺少 model 字段", "type": "invalid_request_error"},
		})
		return
	}
	// 1) 第三方：白名单 / ccs- / <线路>.<模型>
	if _, ok := resolveAggThirdParty(slug); ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.aggDispatch(w, r, "responses")
		return
	}
	// 2) 官方：按本机 Codex 官方 slug 集合判定（兼容 codex-auto-review 等非 gpt- 前缀）
	if codexOfficialSlugSet()[slug] {
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(body))
		h.officialPassthrough(w, r2, body)
		return
	}
	// 3) 都不命中
	recordUsage(usageRec{Line: "codex", Slug: slug, Kind: "responses", Code: http.StatusNotFound})
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{"message": codexUnknownModelMsg + "：" + slug, "type": "unknown_model"},
	})
}
```

- [ ] **Step 9: 注册路由**

在 `hub-go/proxy_agg.go` 的 `/v1/responses` 注册之后（`:329-331` 那段之后）插入：

```go
	// Codex 原生 responses 路径（A′ 方案）：config.toml 只留 openai_base_url 时
	// Codex 把 /responses 打到 /backend-api/codex/responses。GET 同样接受，
	// 因为部分客户端会用 GET 做重试探测。
	codexBackend := func(w http.ResponseWriter, r *http.Request) {
		h.codexBackendResponses(w, r)
	}
	mux.HandleFunc("/backend-api/codex/responses", codexBackend)
```

- [ ] **Step 10: 跑测试确认通过**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestCodexBackend' -count=1 -v .
```

预期输出：5 条 `--- PASS`（`TestCodexBackendRoutesOfficialSlug`、`TestCodexBackendRoutesNonGptOfficialSlug`、`TestCodexBackendRoutesThirdParty`、`TestCodexBackendUnknownModel404`、`TestCodexBackendMissingModel400`），末尾 `ok aihub`。

- [ ] **Step 11: 全量回归 + 提交**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
gofmt -l codex_backend.go codex_backend_test.go proxy_agg.go proxy_agg_thirdparty_test.go proxy_official.go
go vet ./...
go test ./... -count=1
```

预期：`gofmt`/`vet` 无输出；`go test` 末尾 `ok aihub` 且退出码 0（约 48s）。

```bash
cd $REPO
git add hub-go/codex_backend.go hub-go/codex_backend_test.go hub-go/proxy_agg.go \
        hub-go/proxy_agg_thirdparty_test.go hub-go/proxy_official.go
git commit -m "feat(codex): 新增 /backend-api/codex/responses 三分支分流"
```

---

### Task 4: 接管与恢复改为 `openai_base_url` 口径

**Files:**
- Modify: `hub-go/client_takeover.go:49-66`（备份函数加标签）、`:112-217`（`takeOverCodex`）、`:265-315`（`restoreCodex`）、`:444-458`（`apiClientsStatus`）
- Test: `hub-go/client_takeover_test.go`

**Interfaces:**
- Consumes: Task 2 的 `writeCodexMixedCatalog(h *Hub) (string, error)`；现有 `homeJoin`、`backupFile`、`codexCatalogPath`、`aggGatewayBase`、`aggDefaultModel`、`hubShared()`。
- Produces:
  - `func backupFileTagged(path, tag string) (string, error)`
  - `func latestBackup(path, tag string) string`
  - `const codexBasePath = "/backend-api/codex"`、`const codexBaseURL = aggGatewayBase + codexBasePath`
  - `func takeOverCodex() (string, error)`（签名不变，语义改为 A′）
  - `func restoreCodex() (string, error)`（签名不变，改为从最新 `bak-hubgo-*` 还原）
  - `func (h *Hub) apiClientsStatus(...)` 新增 `codex_base_url`、`codex_taken_over`，修 `m[1]` panic

- [ ] **Step 1: 写失败测试**

创建 `hub-go/client_takeover_test.go`：

```go
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexConfigSample = `model = "gpt-5.2"
model_provider = "aihub"
model_reasoning_effort = "high"
model_catalog_json = "/tmp/old-catalog.json"

[model_providers.aihub]
name = "AIHub 统一网关"
base_url = "http://127.0.0.1:8790/v1"
wire_api = "responses"
requires_openai_auth = true

[profiles.official]
model_provider = "openai"
model = "gpt-5.2"
`

// setupCodexHome 造临时 home + 临时数据根 + 写初始 config.toml，并固定 bundled 来源。
func setupCodexHome(t *testing.T) string {
	t.Helper()
	home := isolateUserHome(t)
	isolateOfficialDataRoot(t)
	withBundledJSON(t, bundledSample)
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash", Display: "DeepSeek Flash（Qoder）"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(codexConfigSample), 0600); err != nil {
		t.Fatal(err)
	}
	// auth.json 只写一次，用来断言「接管/还原不动它」
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"probe":"do-not-touch"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestTakeOverCodexWritesOpenAIBaseURL(t *testing.T) {
	home := setupCodexHome(t)
	if _, err := takeOverCodex(); err != nil {
		t.Fatalf("接管失败：%v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `openai_base_url = "http://127.0.0.1:8790/backend-api/codex"`) {
		t.Error("缺少 openai_base_url")
	}
	if strings.Contains(s, `model_provider = "aihub"`) {
		t.Error("顶层 model_provider 必须移除（否则 responses 走 /v1/responses 而不是 /backend-api/codex）")
	}
	if !strings.Contains(s, "model_catalog_json") {
		t.Error("model_catalog_json 必须保留（A′ 下它是选择器唯一入口）")
	}
	if !strings.Contains(s, "[profiles.official]") {
		t.Error("用户 profile 段不许被删")
	}
	// auth.json 不许动
	ab, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != `{"probe":"do-not-touch"}` {
		t.Errorf("auth.json 被改了：%s", ab)
	}
	// 混合目录已生成，且含官方与第三方
	cb, err := os.ReadFile(filepath.Join(home, ".codex", codexCatalogPath))
	if err != nil {
		t.Fatalf("混合目录没生成：%v", err)
	}
	if !strings.Contains(string(cb), "gpt-6.1-sol") || !strings.Contains(string(cb), "qoder.DeepSeek-Flash") {
		t.Error("混合目录应同时含官方与第三方模型")
	}
	// 备份存在
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "config.toml.bak-hubgo-*"))
	if len(matches) != 1 {
		t.Errorf("备份份数 = %d，应为 1", len(matches))
	}
}

func TestRestoreCodexRestoresByteIdentical(t *testing.T) {
	home := setupCodexHome(t)
	cfg := filepath.Join(home, ".codex", "config.toml")
	if _, err := takeOverCodex(); err != nil {
		t.Fatalf("接管失败：%v", err)
	}
	if _, err := restoreCodex(); err != nil {
		t.Fatalf("还原失败：%v", err)
	}
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != codexConfigSample {
		t.Errorf("还原后与接管前不一致：\n--- got ---\n%s\n--- want ---\n%s", b, codexConfigSample)
	}
	// 还原也要备份（用不同后缀，避免下次还原把接管态当成原始态）
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "config.toml.bak-restore-*"))
	if len(matches) != 1 {
		t.Errorf("还原备份份数 = %d，应为 1", len(matches))
	}
	// 再还原一次仍应是原始内容（不会被上一轮的 restore 备份污染）
	if _, err := restoreCodex(); err != nil {
		t.Fatalf("二次还原失败：%v", err)
	}
	b2, _ := os.ReadFile(cfg)
	if string(b2) != codexConfigSample {
		t.Error("二次还原把配置改坏了（restore 备份被当成原始态）")
	}
}

func TestApiClientsStatusReportsBaseURL(t *testing.T) {
	home := setupCodexHome(t)
	// 未接管：codex_base_url 空、codex_taken_over=false
	rec := httptest.NewRecorder()
	(&Hub{}).apiClientsStatus(rec, httptest.NewRequest(http.MethodGet, "/api/clients/status", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("状态不是 JSON：%v", err)
	}
	if out["codex_taken_over"] != false {
		t.Errorf("未接管时 codex_taken_over = %v，应为 false", out["codex_taken_over"])
	}

	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	(&Hub{}).apiClientsStatus(rec2, httptest.NewRequest(http.MethodGet, "/api/clients/status", nil))
	out2 := map[string]any{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	if out2["codex_taken_over"] != true {
		t.Errorf("接管后 codex_taken_over = %v，应为 true", out2["codex_taken_over"])
	}
	if s, _ := out2["codex_base_url"].(string); !strings.Contains(s, "/backend-api/codex") {
		t.Errorf("codex_base_url = %q", s)
	}
	_ = home
}

// 没有 model_provider 行时状态接口不许 panic（旧实现 m[1] 越界）。
func TestApiClientsStatusNoProviderNoPanic(t *testing.T) {
	home := setupCodexHome(t)
	cfg := filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(cfg, []byte("model = \"gpt-5.2\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&Hub{}).apiClientsStatus(rec, httptest.NewRequest(http.MethodGet, "/api/clients/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，应为 200", rec.Code)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestTakeOverCodex|TestRestoreCodex|TestApiClientsStatus' -count=1 .
```

预期输出（关键行）：

```
--- FAIL: TestTakeOverCodexWritesOpenAIBaseURL (0.00s)
    client_takeover_test.go:78: 缺少 openai_base_url
--- FAIL: TestRestoreCodexRestoresByteIdentical (0.00s)
    client_takeover_test.go:120: 还原后与接管前不一致
--- FAIL: TestApiClientsStatusReportsBaseURL (0.00s)
    client_takeover_test.go:151: 未接管时 codex_taken_over = <nil>，应为 false
FAIL
```

- [ ] **Step 3: 备份函数支持标签 + 查最新备份**

把 `hub-go/client_takeover.go:49-66` 的 `backupFile` 替换为：

```go
// backupFileTagged 备份 path 为 <path>.bak-<tag>-<时间戳>，沿用原 mode（取不到用 0600）。
// tag 用来区分「接管前」与「还原前」两类备份：还原只认 bak-hubgo-*，
// 否则第二次还原会把上一轮的还原备份当成原始态，越还原越乱。
func backupFileTagged(path, tag string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err // 文件不存在就没有备份一说
	}
	bak := fmt.Sprintf("%s.bak-%s-%s", path, tag, time.Now().Format("20060102-150405"))
	mode := os.FileMode(0600)
	if st, serr := os.Stat(path); serr == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(bak, b, mode); err != nil {
		return "", err
	}
	return bak, nil
}

// backupFile 接管前备份（保持既有调用方语义与文件名）。
func backupFile(path string) (string, error) { return backupFileTagged(path, "hubgo") }

// latestBackup 返回 <path>.bak-<tag>-* 里最新的一个；没有返回空串。
func latestBackup(path, tag string) string {
	matches, err := filepath.Glob(fmt.Sprintf("%s.bak-%s-*", path, tag))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches) // 时间戳定长，字典序即时间序
	return matches[len(matches)-1]
}
```

并在文件顶部 import 块加入 `"sort"`。

- [ ] **Step 4: 改写 `takeOverCodex`**

把 `hub-go/client_takeover.go:112-217`（从 `// takeOverCodex 文本手术：` 注释起，到 `takeOverCodex` 函数结束的 `}`）整体替换为：

```go
// takeOverCodex A′ 接管（2026-11-09）：把 responses 经统一网关转发，
// 同时把「官方 + 第三方」混合目录写成 Codex 的选择器目录。
//
// 与旧实现（model_provider=aihub + /v1/responses）的区别，实测依据见
// docs/superpowers/specs/2026-11-09-codex-mixed-model-catalog-design.md：
//   - 顶层必须**移除** model_provider：留着它 responses 会走
//     [model_providers.aihub].base_url（/v1/responses），而不是 openai_base_url；
//   - 顶层必须**保留** model_catalog_json：Codex 0.167 的选择器只读它或内置目录，
//     不会来拉 /models（实测 models_hits=0）。
func takeOverCodex() (string, error) {
	path, err := homeJoin(".codex", "config.toml")
	if err != nil {
		return "", err
	}
	catalogPath, err := homeJoin(".codex", codexCatalogPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	bak, _ := backupFile(path)

	lines := strings.Split(string(raw), "\n")
	lines = dropStaleRouterSections(lines)

	hasAihubSection := false
	hasBaseURLLine := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "[model_providers.aihub]" {
			hasAihubSection = true
		}
		if strings.HasPrefix(t, "openai_base_url") {
			hasBaseURLLine = true
		}
	}

	baseComment := "# aihub: Codex 的 responses 经统一网关转发（删除本行与下面 openai_base_url 即恢复官方直连）"
	baseLine := `openai_base_url = "` + codexBaseURL + `"`

	out := make([]string, 0, len(lines)+6)
	topSection := true
	baseWritten := false
	writeBase := func() {
		if baseWritten {
			return
		}
		out = append(out, baseComment, baseLine)
		baseWritten = true
	}
	modelSet := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			if topSection {
				writeBase() // 顶层区结束前补上 openai_base_url
			}
			topSection = false
			out = append(out, ln)
			continue
		}
		if !topSection {
			out = append(out, ln)
			continue
		}
		// 顶层：删 model_provider（关键），改写 openai_base_url / model_catalog_json
		if strings.HasPrefix(t, "model_provider") {
			continue
		}
		if strings.HasPrefix(t, "openai_base_url") {
			writeBase()
			continue
		}
		if strings.HasPrefix(t, "model_catalog_json") {
			out = append(out, `model_catalog_json = "`+catalogPath+`"`)
			continue
		}
		if !modelSet && strings.HasPrefix(t, "model") && strings.Contains(t, "=") &&
			!strings.HasPrefix(t, "model_provider") && !strings.HasPrefix(t, "model_reasoning") &&
			!strings.HasPrefix(t, "model_catalog_json") && !strings.HasPrefix(t, "model_verbosity") {
			out = append(out, `model = "`+aggDefaultModel+`"`)
			modelSet = true
			continue
		}
		out = append(out, ln)
	}
	if topSection {
		writeBase()
	}
	if !hasBaseURLLine {
		// writeBase 已在顶层区结束时写入；这里只处理「整份文件只有 section」的极端情况
		writeBase()
	}
	if !strings.Contains(strings.Join(out, "\n"), "model_catalog_json") {
		out = append(out, `model_catalog_json = "`+catalogPath+`"`)
	}
	res := strings.Join(out, "\n")
	if !hasAihubSection {
		res += "\n" + strings.Join([]string{
			"",
			`[model_providers.aihub]`,
			`name = "AIHub 统一网关"`,
			`base_url = "` + aggGatewayBase + `/v1"`,
			`wire_api = "responses"`,
			`requires_openai_auth = true`,
		}, "\n") + "\n"
	}
	// 原子写：先写 tmp 再 rename，避免写一半留下坏 TOML 让 Codex 起不来。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(res), 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	catalogMsg := ""
	if p, cerr := writeCodexMixedCatalog(hubShared()); cerr == nil {
		catalogMsg = "；混合目录 " + p
	} else {
		catalogMsg = "；⚠ 混合目录生成失败：" + cerr.Error()
	}
	msg := "Codex 已切到统一网关 :8790（openai_base_url=" + codexBaseURL + "，model=" + aggDefaultModel + catalogMsg
	if bak != "" {
		msg += "；原配置已备份为 " + filepath.Base(bak)
	}
	msg += "；重开 Codex 生效）"
	return msg, nil
}
```

- [ ] **Step 5: 改写 `restoreCodex`**

把 `hub-go/client_takeover.go:265-315`（`// restoreCodex 恢复 Codex 官方直连` 注释起至函数结束）整体替换为：

```go
// restoreCodex 恢复 Codex 官方直连。
//
// 2026-11-09（A′）：改为「从最新 bak-hubgo-* 逐字节还原」，这样接管期间被改写的
// model / model_catalog_json / 删掉的 model_provider 都能一字不差回来；
// 还原动作自己的备份用 bak-restore-* 后缀，避免下次还原把还原态当原始态。
// 没有 bak-hubgo-* 时退回文本手术（只删 aihub 加的键），保证老环境也能恢复。
func restoreCodex() (string, error) {
	path, err := homeJoin(".codex", "config.toml")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	if _, err := backupFileTagged(path, "restore"); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if bak := latestBackup(path, "hubgo"); bak != "" {
		bb, rerr := os.ReadFile(bak)
		if rerr != nil {
			return "", fmt.Errorf("读备份 %s 失败：%v", bak, rerr)
		}
		tmp := path + ".tmp"
		if werr := os.WriteFile(tmp, bb, 0644); werr != nil {
			return "", werr
		}
		if rerr := os.Rename(tmp, path); rerr != nil {
			_ = os.Remove(tmp)
			return "", rerr
		}
		return "Codex 已恢复官方直连（从备份 " + filepath.Base(bak) + " 逐字节还原；重开 Codex 生效）", nil
	}

	// 兜底：没有备份时做文本手术，只删 aihub 加的键。
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines))
	topSection := true
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			topSection = false
			out = append(out, ln)
			continue
		}
		if topSection {
			if strings.HasPrefix(t, "openai_base_url") || strings.HasPrefix(t, "model_provider") ||
				strings.HasPrefix(t, "model_catalog_json") {
				continue
			}
			if strings.HasPrefix(t, "# aihub:") {
				continue
			}
			if strings.HasPrefix(t, "model") && strings.Contains(t, "=") &&
				!strings.HasPrefix(t, "model_reasoning") {
				if strings.Contains(t, `"intl.`) || strings.Contains(t, `"dom.`) ||
					strings.Contains(t, `"qoder.`) || strings.Contains(t, `"trae.`) ||
					strings.Contains(t, `"zcode.`) || strings.Contains(t, `"teleagent.`) ||
					strings.Contains(t, `"oc.`) {
					continue
				}
			}
		}
		out = append(out, ln)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")), 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return "Codex 已恢复官方直连（无备份可还原，已删除网关相关键；重开 Codex 生效）", nil
}
```

- [ ] **Step 6: 修 `apiClientsStatus`**

把 `hub-go/client_takeover.go:444-458` 的 `apiClientsStatus` 整体替换为：

```go
// apiClientsStatus 客户端接管状态。
// 2026-11-09（A′）：口径从 model_provider 改为 openai_base_url；
// 同时修掉旧实现 m[1] 无匹配时的越界 panic。
func (h *Hub) apiClientsStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true}
	if p, err := homeJoin(".codex", "config.toml"); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			s := string(b)
			baseURL := ""
			if m := regexp.MustCompile(`(?m)^\s*openai_base_url\s*=\s*"([^"]*)"`).FindStringSubmatch(s); len(m) > 1 {
				baseURL = m[1]
			}
			provider := ""
			if m := regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]*)"`).FindStringSubmatch(s); len(m) > 1 {
				provider = m[1]
			}
			out["codex_base_url"] = baseURL
			out["codex_provider"] = provider // 保留旧字段，前端还有兼容分支
			out["codex_taken_over"] = strings.Contains(baseURL, "/backend-api/codex")
		}
	}
	out["dsh"] = dshTakeoverStatus()
	writeJSON(w, http.StatusOK, out)
}
```

- [ ] **Step 7: 跑测试确认通过**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestTakeOverCodex|TestRestoreCodex|TestApiClientsStatus' -count=1 -v .
```

预期输出：4 条 `--- PASS`，末尾 `ok aihub`。

- [ ] **Step 8: 全量回归 + 提交**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
gofmt -l client_takeover.go client_takeover_test.go
go vet ./...
go test ./... -count=1
```

预期：无输出 / 退出码 0 / `ok aihub`（约 48s）。

```bash
cd $REPO
git add hub-go/client_takeover.go hub-go/client_takeover_test.go
git commit -m "feat(codex): 接管改为 openai_base_url + 混合目录，恢复改为从备份逐字节还原"
```

---

### Task 5: 控制台文案与状态口径

**Files:**
- Modify: `hub-go/assets/console.html:764`（文案）、`:1402`、`:1419`、`:2591`（状态判定）
- Test: `hub-go/console_codex_takeover_test.go`

**Interfaces:**
- Consumes: Task 4 的 `/api/clients/status` 新字段 `codex_base_url`、`codex_taken_over`；`consoleSource(t *testing.T) string`（`hub-go/testguard_test.go:42`）。
- Produces: 无新 Go 符号（纯前端）。

- [ ] **Step 1: 写失败测试**

创建 `hub-go/console_codex_takeover_test.go`：

```go
package main

import (
	"strings"
	"testing"
)

// 接管状态必须按 openai_base_url 判定（A′），不能再看 model_provider。
func TestConsoleCodexTakeoverUsesBaseURL(t *testing.T) {
	src := consoleSource(t)
	if !strings.Contains(src, "clients.codex_taken_over") {
		t.Error("总览/设置页应读 clients.codex_taken_over")
	}
	if !strings.Contains(src, "clients.codex_base_url") {
		t.Error("设置页应展示 clients.codex_base_url")
	}
	if strings.Contains(src, "clients.codex_provider === 'aihub'") {
		t.Error("旧的 provider==aihub 判定必须去掉（A′ 下顶层不再写 model_provider）")
	}
	if strings.Contains(src, "d.codex_provider === 'aihub'") {
		t.Error("设置页旧判定也要去掉")
	}
}

// 文案要说明新机制，避免用户按旧文档理解。
func TestConsoleCodexTakeoverCopy(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{"openai_base_url", "混合目录", "官方登录身份"} {
		if !strings.Contains(src, want) {
			t.Errorf("接管文案应包含 %q", want)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestConsoleCodexTakeover' -count=1 .
```

预期输出：

```
--- FAIL: TestConsoleCodexTakeoverUsesBaseURL (0.00s)
    console_codex_takeover_test.go:12: 总览/设置页应读 clients.codex_taken_over
    console_codex_takeover_test.go:20: 旧的 provider==aihub 判定必须去掉（A′ 下顶层不再写 model_provider）
--- FAIL: TestConsoleCodexTakeoverCopy (0.00s)
    console_codex_takeover_test.go:31: 接管文案应包含 "openai_base_url"
FAIL
```

- [ ] **Step 3: 改文案**

把 `hub-go/assets/console.html:764` 那段 `<span class="hint">…</span>` 的首行改为：

```html
          <span class="hint">接管 = 把 Codex 的 responses 指向 :8790/backend-api/codex（顶层写 openai_base_url、去掉 model_provider），并把「官方模型 + 第三方模型」写成一份混合目录供选择器读取；官方登录身份（auth.json）全程不动、仍由 Codex 自己携带。恢复官方 = 从接管前的备份逐字节还原。均自动备份，改动重开客户端生效。<br>
```

- [ ] **Step 4: 改总览状态判定**

把 `hub-go/assets/console.html:1402` 的：

```js
  const codexOk = clients.codex_provider === 'aihub';
```

改为：

```js
  const codexOk = clients.codex_taken_over === true;
```

并把 `:1404` 改为：

```js
  if (codexEl) codexEl.textContent = clients.codex_taken_over ? '✓ 已接管' : (clients.codex_base_url ? ('未接管（' + clients.codex_base_url + '）') : '—');
```

并把 `:1419` 的文案改为：

```js
    t.textContent = '⚠ 网关运行中，但 Codex 未接管（base_url ' + (clients.codex_base_url || '—') + '）';
```

- [ ] **Step 5: 改设置页状态行**

把 `hub-go/assets/console.html:2591` 的：

```js
      if (codex) codex.textContent = d.codex_provider === 'aihub' ? 'provider=aihub ✓ 已接管' : ('provider=' + (d.codex_provider || '—') + '（未接管）');
```

改为：

```js
      if (codex) codex.textContent = d.codex_taken_over ? ('openai_base_url=' + d.codex_base_url + ' ✓ 已接管') : ('base_url=' + (d.codex_base_url || '—') + '（未接管）');
```

- [ ] **Step 6: 跑测试确认通过**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
go test -run 'TestConsoleCodexTakeover|TestConsole' -count=1 -v .
```

预期输出：`TestConsoleCodexTakeoverUsesBaseURL`、`TestConsoleCodexTakeoverCopy` 与既有 `TestConsole*` 全部 `--- PASS`，末尾 `ok aihub`。

- [ ] **Step 7: 提交**

```bash
cd $REPO
git add hub-go/assets/console.html hub-go/console_codex_takeover_test.go
git commit -m "feat(控制台): Codex 接管状态改按 openai_base_url 判定并更新文案"
```

---

### Task 6: 删残留常量、全量回归、构建与交付

**Files:**
- Modify: `hub-go/proxy_official.go:25-27`（删 `officialCatalogModels`）
- Test: 无新增（跑全量）

**Interfaces:**
- Consumes: 前五个任务的全部产物。
- Produces: 可交付的 `hub-go/bin/aihub`。

- [ ] **Step 1: 确认残留常量真的零引用**

```bash
cd $REPO/hub-go
grep -rn "officialCatalogModels" --include=*.go .
```

预期输出：只有 `proxy_official.go:27` 的定义行（以及可能的一行注释）。若出现其它引用，**停止并汇报**，不要删。

- [ ] **Step 2: 删掉残留常量**

把 `hub-go/proxy_official.go:25-27` 三行：

```go
// officialCatalogModels 模型选择器里展示的官方模型（请求侧不做白名单校验：
// gpt- 前缀一律透传，官方没这个名字时由官方 API 报错，避免网关侧失真）。
var officialCatalogModels = []string{"gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.6-sol"}
```

整体删除（替换为空）。

- [ ] **Step 3: 全量回归**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
gofmt -l .
go vet ./...
go test ./... -count=1
```

预期：`gofmt -l .` 无输出（若报既有历史文件，**不要顺手格式化**，只确认新增/修改文件干净：`gofmt -l codex_official.go codex_catalog.go codex_backend.go client_takeover.go proxy_agg.go proxy_official.go`）；`go vet` 退出码 0；`go test` 末尾 `ok aihub`（约 48s）。

- [ ] **Step 4: 构建二进制**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
./scripts/build.sh
ls -la --time-style=+%Y-%m-%d\ %H:%M:%S bin/aihub
```

预期：`bin/aihub` 时间戳为刚刚；无编译错误。

- [ ] **Step 5: 预览实例端到端（不碰线上）**

```bash
export PATH=/usr/local/go/bin:$PATH
cd $REPO/hub-go
ROOT=/tmp/aihub-codex-mixed && rm -rf "$ROOT" && mkdir -p "$ROOT"
AIHUB_ROOT="$ROOT" ./bin/aihub -no-browser -port 8831 >"$ROOT/preview.log" 2>&1 &
sleep 2
curl -s -o /dev/null -w 'health=%{http_code}\n' http://127.0.0.1:8831/health
curl -s -X POST http://127.0.0.1:8831/api/clients/takeover -H 'Content-Type: application/json' -d '{"app":"codex"}'
```

预期：`health=200`；接管返回 `{"ok":true,"msg":"Codex 已切到统一网关 :8790（openai_base_url=...`（**注意**：此步会改真实 `~/.codex/config.toml`，只在用户明确同意「现在就接管」时才执行；否则跳过本步，改用任务 4 的单元测试作为验收）。

- [ ] **Step 6: PII 扫描**

扫描模式清单放在**本地不入库**的文件 `.tmp/pii-patterns.txt`（每行一个模式，仓库 `.gitignore` 已忽略 `.tmp/`）：

```bash
cd $REPO
mkdir -p .tmp
[ -f .tmp/pii-patterns.txt ] || cat > .tmp/pii-patterns.txt <<'EOF'
PLACEHOLDER_ABS_HOME
PLACEHOLDER_GITHUB_ACCOUNT
PLACEHOLDER_USER_ACCOUNT
PLACEHOLDER_HOSTNAME
PLACEHOLDER_QODER_ACCOUNT_A
PLACEHOLDER_QODER_ACCOUNT_B
EOF
while IFS= read -r pat; do
  printf '%-32s %s\n' "$pat" "$(git log --all -S"$pat" --oneline | wc -l)"
done < .tmp/pii-patterns.txt
git status --porcelain=v1
```

首次使用时，把 `.tmp/pii-patterns.txt` 里的六个 `PLACEHOLDER_*` 换成你本机已知的真实值（本计划刻意不内联它们，避免把 PII 写进公开仓库）。

预期：六行计数全部 `0`；`git status` 只剩本次改动文件（或为空，若已提交）。

- [ ] **Step 7: 提交并给出用户操作清单**

```bash
cd $REPO
git add -A hub-go/proxy_official.go
git commit -m "chore(codex): 删除零调用的官方目录残留常量"
git --no-pager log --oneline -6
```

预期：6 条提交，最新为本次；工作区 0 改动。

给用户的操作清单（写进最终回复，不代替用户执行）：

1. 重启 aihub（加载新二进制）：`systemctl --user stop aihub-console` → `cd $REPO/hub-go && ./scripts/start.sh -no-browser`。
2. 控制台「设置 → 客户端接管 → Codex → 接管」（生成混合目录 + 改 config.toml）。
3. **重开 Codex**（桌面版只在启动时读目录）。
4. 验收：`/model` 里同时看到官方模型与第三方模型；各选一个发一句话。
5. 回滚：控制台点「恢复官方」，或手动 `cp ~/.codex/config.toml.bak-hubgo-<时间戳> ~/.codex/config.toml`。

---

## Self-Review

**1. Spec coverage**

| 规格条目 | 落地任务 |
|---|---|
| 4.1 顶层加 `openai_base_url`、移除 `model_provider`、保留 `model_catalog_json`、不碰 `auth.json` | Task 4（Step 4 实现，Step 1 测试断言） |
| 4.2 只新增 `POST /backend-api/codex/responses`，不新增 `/models`、不新增 `model_catalog_url` | Task 3 |
| 4.2 抽 `resolveAggThirdParty`，`resolveAggModel` 行为不变 | Task 3（Step 3 + 等价性测试） |
| 4.2 删 `officialCatalogModels`、不删 `aggPrettyNames` | Task 6（Step 1 grep 门禁） |
| 4.3 回归固定「无 `model_provider` 时 responses 落 `/backend-api/codex/responses`」 | Task 4 测试断言配置行 + Task 3 路由测试 |
| 5.1 官方条目取 `codex debug models --bundled`、`AIHUB_CODEX_BIN` 可覆盖 | Task 1 |
| 5.1 第三方条目复用 `codexModelEntry` 模板逻辑 | Task 2（Step 3 抽函数） |
| 5.1 合并顺序 list → hide → 第三方、priority 连续 | Task 2 测试 |
| 5.1 tmp+rename、空表拒写、写前备份 | Task 2 实现 + `TestWriteCodexMixedCatalogRefusesEmpty` |
| 5.1 官方 slug 集合落 `codex_official_slugs.json` 0600 | Task 1 测试断言 0600 |
| 5.1 回退 `models_cache.json` | Task 2 `TestWriteCodexMixedCatalogUsesModelsCacheFallback` |
| 5.1 4 MiB / 200 条上限 | Task 2 `TestWriteCodexMixedCatalogCapsEntries` + 字节上限检查 |
| 5.2 三分支分流、404 unknown_model、不猜测路由 | Task 3 四条测试 |
| 5.2 官方 slug 集合缺失时现场只读重算 | Task 1 `TestCodexOfficialSlugSetFallsBackToBundle` |
| 5.3 非目标（不做 `/models`、不做按账号缓存、不读 auth.json） | 全篇未实现即符合 |
| 6 组件边界（`codex_backend.go` 不写配置、`client_takeover.go` 不碰官方请求） | 文件划分即约束 |
| 7 错误处理：透传失败原样回、TOML 合法、备份可回退、无裸 client | Task 3/4/6 |
| 8.1 单元测试清单 | Task 1/2/3 |
| 8.2 配置测试（临时 `CODEX_HOME`、逐字节还原、auth.json 不变） | Task 4 |
| 8.3 全量回归、预览、真实验收交给用户 | Task 6 |
| 9 交付顺序 | 任务 1→6 顺序一致 |
| 10 已核实事实（行号/签名） | 各任务 Files 段引用 |

无遗漏。

**2. Placeholder scan**

本计划不含 `TBD`/`TODO`/`implement later`/`fill in details`/`Add appropriate error handling`/`Write tests for the above`/`Similar to Task N`；每个代码步骤都有完整可编译代码块；每个测试步骤都有可粘贴的断言；每个命令步骤都有预期输出。

**3. Type consistency**

- `codexOfficialEntries() ([]map[string]any, error)`：Task 1 定义，Task 2/3 使用，签名一致。
- `codexOfficialSlugSet() map[string]bool`：Task 1 定义，Task 3 使用。
- `codexThirdPartyEntries(h *Hub) ([]map[string]any, error)`：Task 2 定义并被 `writeCodexCatalogFile`/`writeCodexMixedCatalog` 共用。
- `writeCodexMixedCatalog(h *Hub) (string, error)`：Task 2 定义，Task 4 调用。
- `resolveAggThirdParty(slug string) (aggModel, bool)`：Task 3 定义，Task 3 的 `codexBackendResponses` 使用。
- `officialBackendURLVar`：Task 3 定义（变量），Task 3 测试覆盖。
- `backupFileTagged(path, tag string) (string, error)` / `latestBackup(path, tag string) string`：Task 4 定义并使用。
- 前端字段 `codex_taken_over` / `codex_base_url`：Task 4 后端产出，Task 5 前端消费，命名一致。

---

## Execution Handoff

计划已保存到 `docs/superpowers/plans/2026-11-09-codex-mixed-model-catalog.md`。两种执行方式：

**1. Subagent-Driven（推荐）** — 每个任务派一个全新 subagent，任务间我来 review，迭代快、上下文干净。

**2. Inline Execution** — 在当前会话按 `executing-plans` 分批执行，带检查点。

选哪种？
