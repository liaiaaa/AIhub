package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 缺陷 A：config 有 section 但顶层没有 model_catalog_json 时，
// 追加的 model_catalog_json 会落到最后一个 section 里面（变成 profile 的键）。
func TestTakeOverCodexKeepsCatalogLineTopLevel(t *testing.T) {
	home := isolateUserHome(t)
	isolateOfficialDataRoot(t)
	withBundledJSON(t, bundledSample)
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash", Display: "D"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 注意：顶层没有 model_catalog_json，只有 section
	cfg := "model = \"gpt-5.2\"\n\n[model_providers.aihub]\nbase_url = \"http://127.0.0.1:8790/v1\"\n\n[profiles.official]\nmodel_provider = \"openai\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := takeOverCodex(); err != nil {
		t.Fatalf("接管失败：%v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	s := string(b)
	firstSection := strings.Index(s, "\n[")
	catalogIdx := strings.Index(s, "model_catalog_json")
	if catalogIdx > firstSection && firstSection > 0 {
		t.Errorf("model_catalog_json 落在了 section 里（位置 %d > 首个 section %d）：\n%s", catalogIdx, firstSection, s)
	}
}

// 缺陷 B：连续接管两次后还原，应回到最初配置（而不是回到「已接管」状态）。
func TestDoubleTakeoverThenRestoreReturnsOriginal(t *testing.T) {
	home := isolateUserHome(t)
	isolateOfficialDataRoot(t)
	withBundledJSON(t, bundledSample)
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash", Display: "D"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(orig), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	if _, err := takeOverCodex(); err != nil { // 第二次
		t.Fatal(err)
	}
	if _, err := restoreCodex(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if strings.Contains(string(b), "openai_base_url") {
		t.Errorf("连续接管两次后还原，仍残留 openai_base_url（还原到了已接管状态）：\n%s", b)
	}
}
