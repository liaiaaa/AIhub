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
	setupCodexHome(t)
	// 未接管：codex_taken_over=false
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
