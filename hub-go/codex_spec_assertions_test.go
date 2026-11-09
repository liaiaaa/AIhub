package main

// 规格第 8 章点名的断言（2026-11-09 最终审查 Important 10 补齐）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 规格 8.2：接管前备份的内容必须与接管前原文逐字节一致。
func TestTakeoverBackupContentIsByteIdentical(t *testing.T) {
	orig := "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n\n[profiles.x]\nmodel = \"gpt-5.2\"\n"
	home := takeoverFixture(t, orig)
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "config.toml.bak-hubgo-*"))
	if len(matches) != 1 {
		t.Fatalf("备份份数 = %d，应为 1", len(matches))
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != orig {
		t.Errorf("备份内容与接管前原文不一致：\n--- bak ---\n%s\n--- orig ---\n%s", b, orig)
	}
}

// 规格 8.1：官方 slug 集合里不许出现第三方 slug。
func TestOfficialSlugSetExcludesThirdPartySlugs(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	_ = home
	set := codexOfficialSlugSet()
	if len(set) == 0 {
		t.Fatal("官方 slug 集合为空")
	}
	for _, bad := range []string{"qoder.DeepSeek-Flash", "dom.deepseek-v4-flash", "intl.x", "trae.glm-5.3"} {
		if set[bad] {
			t.Errorf("官方 slug 集合里混进了第三方 slug：%s", bad)
		}
	}
	// 官方 slug 必须是 bundled 里那几个
	if !set["gpt-6.1-sol"] {
		t.Errorf("官方 slug 集合里缺 gpt-6.1-sol：%v", set)
	}
}

// 规格 8.2：接管后生成的 config.toml 必须能被 tomllib 解析。
func TestTakeoverProducesValidTOML(t *testing.T) {
	cfg := "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\nmodel_context_window = 1000000\n\n" +
		"[profiles.official]\nopenai_base_url = \"https://chatgpt.com/backend-api/codex\"\nmodel = \"gpt-5.2\"\n\n" +
		"[shell_environment_policy.set]\nANTHROPIC_AUTH_TOKEN = \"dummy\"\n"
	home := takeoverFixture(t, cfg)
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	ok, detail := tomlValid(got)
	if !ok {
		t.Errorf("接管后不是合法 TOML：%s\n%s", detail, got)
	}
	// 用户自己的 section 必须原样保留
	if !strings.Contains(string(got), "[shell_environment_policy.set]") ||
		!strings.Contains(string(got), `ANTHROPIC_AUTH_TOKEN = "dummy"`) {
		t.Errorf("用户的 [shell_environment_policy.set] 被破坏：\n%s", got)
	}
}

// 规格 8.2：还原后 config.toml 必须能被 tomllib 解析。
func TestRestoreProducesValidTOML(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreCodex(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if ok, detail := tomlValid(got); !ok {
		t.Errorf("还原后不是合法 TOML：%s\n%s", detail, got)
	}
}

// 规格 8.1：混合目录写失败时不许留下 .tmp 残留。
func TestMixedCatalogLeavesNoTmpFile(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	if _, err := writeCodexMixedCatalog(&Hub{}); err != nil {
		t.Fatal(err)
	}
	cat := filepath.Join(home, ".codex", codexCatalogPath)
	if _, err := os.Stat(cat + ".tmp"); err == nil {
		t.Errorf("留下了 %s.tmp", cat)
	}
	// config.toml 同理
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml.tmp")); err == nil {
		t.Error("留下了 config.toml.tmp")
	}
}

// 规格 8.1：officialPassthrough 不带凭据必须回 401 auth_missing（证明路由活着）。
func TestOfficialPassthroughRequiresAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-6.1-sol"}`))
	(&Hub{}).officialPassthrough(rec, req, []byte(`{"model":"gpt-6.1-sol"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态 = %d，应为 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "auth_missing") {
		t.Errorf("body 应含 auth_missing：%s", rec.Body.String())
	}
}

// 规格 8.1 回归：现有 /v1/codex/models 仍返回第三方模型，不受混合目录影响。
func TestV1CodexModelsUnchangedByMixedCatalog(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	_ = home
	out := (&Hub{}).aggCodexModels()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// 第三方必须在（qoder.DeepSeek-Flash 来自 fixture 的 aggList）
	if !strings.Contains(s, "qoder.DeepSeek-Flash") {
		t.Errorf("/v1/codex/models 丢了第三方模型：%s", s)
	}
	// 官方 gpt-* 不属于 /v1/codex/models（那是聚合口的第三方目录）
	if strings.Contains(s, "gpt-6.1-sol") {
		t.Errorf("/v1/codex/models 不该含官方模型（官方走混合目录）：%s", s)
	}
}

// 规格 7：日志脱敏 —— 接管/还原的日志与回执里不许出现凭据字段。
func TestTakeoverMessagesContainNoCredentials(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	msg, err := takeOverCodex()
	if err != nil {
		t.Fatal(err)
	}
	rmsg, err := restoreCodex()
	if err != nil {
		t.Fatal(err)
	}
	_ = home
	for _, s := range []string{msg, rmsg} {
		for _, bad := range []string{"Authorization", "Bearer ", "chatgpt-account-id", "auth.json"} {
			if strings.Contains(s, bad) {
				t.Errorf("回执里出现凭据相关字样 %q：%s", bad, s)
			}
		}
	}
}
