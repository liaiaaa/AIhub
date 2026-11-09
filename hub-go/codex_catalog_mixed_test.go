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
	if want := filepath.Join(home, ".codex", codexCatalogPath); path != want {
		t.Fatalf("落盘路径 = %q，应为 %q", path, want)
	}
	models := readMixedCatalog(t, path)
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

// 字节上限触发时必须逐条截断（而不是报错），且截断后仍非空。
// 真实官方条目单条 37–87 KB，4 MiB 往往比 200 条更早触发。
func TestWriteCodexMixedCatalogCapsBytes(t *testing.T) {
	setupMixedEnv(t)
	big := strings.Repeat("x", 400*1024) // 每条 400 KB，12 条合计 4.8 MB
	var sb strings.Builder
	sb.WriteString(`{"models":[`)
	for i := 0; i < 12; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"slug":"gpt-big-`)
		sb.WriteString(itoaForTest(i))
		sb.WriteString(`","display_name":"X","visibility":"list","description":"`)
		sb.WriteString(big)
		sb.WriteString(`"}`)
	}
	sb.WriteString(`]}`)
	withBundledJSON(t, sb.String())

	path, err := writeCodexMixedCatalog(&Hub{})
	if err != nil {
		t.Fatalf("字节超限应截断而非报错：%v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > codexMixedMaxBytes {
		t.Errorf("文件 %d 字节仍超上限 %d", st.Size(), codexMixedMaxBytes)
	}
	models := readMixedCatalog(t, path)
	if len(models) == 0 {
		t.Fatal("截断后不能为空（空表 Codex 打不开）")
	}
	if p, _ := models[0]["priority"].(float64); int(p) != 1 {
		t.Errorf("截断后首条 priority = %v，应为 1", models[0]["priority"])
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
