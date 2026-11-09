package main

// Codex 桌面版模型目录文件（2026-10-13）。
//
// 背景：Codex 的 app-server（桌面版/编辑器插件）只在启动时读一次模型目录。
// CLI 走 provider 的 /v1/models，桌面版还认 config.toml 顶层 `model_catalog_json`
// 指向的本地目录文件（cc-switch 时代的机制，实测有效）。之前 takeOverCodex 把
// 这行注释掉、指望桌面版来拉网关目录——实测桌面版根本不来拉，选择器只剩
// 官方推荐模型集。
//
// 本文件把统一网关的模型目录（hub-go 各线路模型）写成
// ~/.codex/aihub-model-catalog.json，条目结构用 33 键完整模板（桌面版已验证接受）。
// 2026-10-15：模板改为仓库内置 + embed 内联双保险，不再依赖 cc-switch 遗留文件。

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// assets 仓库资产（图标、目录模板等）。embed 保证二进制自包含，
// 不依赖运行时的相对路径或外部文件。
//
//go:embed all:assets
var assets embed.FS

const codexCatalogPath = "aihub-model-catalog.json"

var catalogMu sync.Mutex

// catalogTemplate 取目录条目模板（33 键完整形态，Codex 桌面版只接受这种结构）。
//
// 2026-10-15 改造：原来只读 ~/.codex/cc-switch-model-catalog.json（cc-switch 时代
// 产物）。那是外部文件，一旦被清理桌面版目录就生成不了。现在改成三级回退：
//  1. 仓库内置 hub-go/assets/codex-catalog-template.json（随二进制发布，主路径）
//  2. 编译期内联 embeddedCatalogTemplate（embed.FS，兜底）
//  3. ~/.codex/cc-switch-model-catalog.json（cc-switch 遗留，兼容用）
func catalogTemplate() []map[string]any {
	// 1+2：仓库内置（embed.FS，编译期内联）
	if b, err := assets.ReadFile("assets/codex-catalog-template.json"); err == nil {
		if tpl := parseCatalogTemplate(b); tpl != nil {
			return tpl
		}
	}
	// 3：cc-switch 遗留文件（兼容）
	path, err := homeJoin(".codex", "cc-switch-model-catalog.json")
	if err == nil {
		if b, err := os.ReadFile(path); err == nil {
			if tpl := parseCatalogTemplate(b); tpl != nil {
				return tpl
			}
		}
	}
	return nil
}

func parseCatalogTemplate(b []byte) []map[string]any {
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if json.Unmarshal(b, &doc) != nil || len(doc.Models) == 0 {
		return nil
	}
	return doc.Models
}

// catalogGroupOrder 选择器里的分组顺序（用户要求：国内、国外、Qoder、官方…）。
// priority 重编为组内连续编号，Codex 按它排序，同名组就挨在一起。
var catalogGroupOrder = []string{"dom", "intl", "qoder", "trae", "zcode", "official"}

func catalogGroupRank(line string) int {
	for i, l := range catalogGroupOrder {
		if l == line {
			return i
		}
	}
	return len(catalogGroupOrder)
}

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

func cloneJSONMap(m map[string]any) (map[string]any, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// codexCatalogRefresh 白名单变化后异步刷新桌面版目录文件（失败只记日志，
// 不打断增删操作）。sharedHub 未就绪时跳过。
func codexCatalogRefresh() {
	if sharedHub == nil {
		return
	}
	go func() {
		defer recoverGoroutine("codex_catalog.go:210")
		if _, err := writeCodexCatalogFile(sharedHub); err != nil {
			logHub("桌面版目录文件刷新失败：%v", err)
		}
	}()
}

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
	// 字节上限也要「截断」而不是报错：单条官方条目实测 37–87 KB，
	// 4 MiB 往往比 200 条更早触发。从尾部（第三方 → 官方 hide）往前丢，
	// 丢掉后重新编号 priority，保证截断后仍非空。
	beforeTruncate := len(entries)
	for len(out) > codexMixedMaxBytes && len(entries) > 1 {
		entries = entries[:len(entries)-1]
		for i, e := range entries {
			e["priority"] = i + 1
		}
		out, err = json.MarshalIndent(map[string]any{"models": entries}, "", " ")
		if err != nil {
			return "", err
		}
	}
	if len(entries) < beforeTruncate {
		// 只记一次：逐条记会刷出上百行日志（最多可丢 133 条）
		logHub("混合目录超 4 MiB：%d → %d 条", beforeTruncate, len(entries))
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
