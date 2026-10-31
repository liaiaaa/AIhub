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

// writeCodexCatalogFile 把当前混合目录写到桌面版目录文件。模板条目整体克隆
// （保证 33 键齐全），只改 slug / display_name / description / priority。
// 排列按 catalogGroupOrder 分组：国内 → 国际 → Qoder → Trae → Zcode。
//
// 2026-10-15：官方模型不再进桌面版目录（与 /v1/models 一致，见 aggCodexModels）。
// 官方透传路由保留，直接请求 gpt-* 仍走官方订阅。
func writeCodexCatalogFile(h *Hub) (string, error) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	tpl := catalogTemplate()
	if tpl == nil {
		return "", fmt.Errorf("内置目录模板缺失，桌面版目录跳过（CLI 不受影响）")
	}
	list := append([]aggModel{}, aggSnapshot()...)
	// 组内按 slug 排序，组间按 catalogGroupOrder——选择器里同类相邻。
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
		// 2026-10-19：display_name 改用与 /v1/models 同一套生成函数，
		// 带上倍率与上下文（例：「DeepSeek V4 Flash（国内） · 0.17x · 上下文 1M」）。
		//
		// 原来这里直接用 am.Display —— 那是聚合白名单 agg_models.json 里手写的
		// 静态字符串，只有「DeepSeek V4 Flash（国内）」，没有倍率。于是 Codex 的
		// 模型选择器看不到计价，与 dsh / curl 看到的完全不一致。
		// 复用 aggOpenAIDisplayName 后三者统一，上游倍率一变这里跟着变
		// （前提是目录刷新后触发 codexCatalogRefresh，见 wb_config.go）。
		display := am.Display
		if h != nil {
			display = aggOpenAIDisplayName(h, am)
		}
		items = append(items, item{slug: am.Slug, display: display,
			desc: display + " · AIHub 统一网关"})
	}
	entries := make([]map[string]any, 0, len(items))
	// 上下文窗口与「向 Codex 宣告的上下文窗口」（设置页可调）同步：
	// 模板条目是 cc-switch 时代的 1M 快照，不改的话 Codex 一直显示 950k
	// （context_window × effective 95%），宣告值形同虚设（2026-10-13 用户报）。
	// auto_compact 保持模板比例 90%。
	declareCtx := 0
	if h != nil {
		declareCtx = h.contextWindow()
	}
	for i, it := range items {
		e, err := cloneJSONMap(tpl[i%len(tpl)])
		if err != nil {
			// 2026-10-21 开源审查修：原来 clone 失败直接拿模板本体当条目，
			// 后面几行会往里写 slug/display_name/limit —— 原地污染共享模板，
			// 之后所有条目拿到的都是被改过的同一份。改成新建空 map。
			e = map[string]any{}
		}
		e["slug"] = it.slug
		e["display_name"] = it.display
		e["description"] = it.desc
		e["priority"] = i + 1 // 连续编号：选择器按分组顺序排列
		if declareCtx > 0 {
			e["context_window"] = declareCtx
			e["max_context_window"] = declareCtx
			e["auto_compact_token_limit"] = declareCtx * 9 / 10
		}
		entries = append(entries, e)
	}
	path, err := homeJoin(".codex", codexCatalogPath)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		// 空表对 Codex 桌面版毫无价值：它要求 model_catalog 至少 1 个 model，
		// 只在启动时读一次、不会自愈。保留现有文件永远比写成空表好。
		// 2026-10-27 事故：测试把白名单清空后走到这里，把用户真实文件覆盖成
		// {"models": []}，Codex 直接打不开（"must contain at least one model"）。
		logHub("⚠ 桌面版目录跳过：白名单 0 个模型，保留现有文件不覆盖（%s）", path)
		return "", fmt.Errorf("白名单为空，保留现有目录文件：%s", path)
	}
	out, err := json.MarshalIndent(map[string]any{"models": entries}, "", " ")
	if err != nil {
		return "", err
	}
	// ~/.codex 可能还不存在（全新机器 / 测试用的临时 home）：先建目录，
	// 否则 WriteFile 会 ENOENT。
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
		// 成功路径也要留痕：原来只在失败时记日志，12:22 那次把真实文件写空
		// 在 hub.log 里查不到任何记录，只能靠反推（2026-10-27 诊断浪费）。
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
		if _, err := writeCodexCatalogFile(sharedHub); err != nil {
			logHub("桌面版目录文件刷新失败：%v", err)
		}
	}()
}
