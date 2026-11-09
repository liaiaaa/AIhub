package main

// takeOverCodex / restoreCodex 的回归测试（2026-11-09 最终审查修复）。
//
// 每条测试对应一个已复现的真实缺陷，命名里带编号方便回溯审查报告。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// takeoverFixture 准备临时 home + 临时数据根 + 固定 bundled/aggList，并写入指定 config.toml。
func takeoverFixture(t *testing.T, cfg string) string {
	t.Helper()
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
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func readCfg(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 缺陷 1（Critical）：接管后任何一次白名单变更都会走 codexCatalogRefresh，
// 它必须写「混合目录」，不能退回「只写第三方」——否则官方模型从 /model 消失。
func TestCodexCatalogRefreshKeepsOfficialEntries(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(home, ".codex", codexCatalogPath)
	before, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatalf("接管后应有混合目录：%v", err)
	}
	if !strings.Contains(string(before), "gpt-6.1-sol") {
		t.Fatal("接管后混合目录里应有官方条目")
	}

	// 白名单变更 → codexCatalogRefresh 的后台路径（同步调它内部用的函数，避免测异步）
	if _, err := writeCodexMixedCatalog(hubShared()); err != nil {
		t.Logf("刷新返回：%v", err)
	}
	after, err := os.ReadFile(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "gpt-6.1-sol") {
		t.Errorf("刷新后官方条目消失（说明刷新走了「只写第三方」的旧路径）：\n%s", truncateForLog(string(after)))
	}
}

// 缺陷 2（Critical）：顶层已有 model_context_window 等键时，接管不许把它们改写成 model 行。
func TestTakeOverCodexKeepsModelPrefixedKeys(t *testing.T) {
	home := takeoverFixture(t,
		"model_context_window = 1000000\nmodel = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	got := readCfg(t, home)
	if n := strings.Count(got, "model = \""); n != 1 {
		t.Errorf("顶层 model = 行数 = %d，应为 1（model_context_window 被误改写成 model 行）：\n%s", n, got)
	}
	if !strings.Contains(got, "model_context_window = 1000000") {
		t.Errorf("model_context_window 被改写没了：\n%s", got)
	}
}

// 缺陷 4（Critical）：[profiles.*] 段里有 openai_base_url 时，
// 顶层目录指针不能被插进那个 profile 段。
func TestTakeOverCodexCatalogLineStaysTopLevelWithProfileBaseURL(t *testing.T) {
	home := takeoverFixture(t,
		"model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n\n"+
			"[profiles.official]\nopenai_base_url = \"https://chatgpt.com/backend-api/codex\"\nmodel = \"gpt-5.2\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	got := readCfg(t, home)
	firstSection := strings.Index(got, "\n[")
	catalogIdx := strings.Index(got, "model_catalog_json")
	if catalogIdx < 0 {
		t.Fatalf("没有写 model_catalog_json：\n%s", got)
	}
	if firstSection > 0 && catalogIdx > firstSection {
		t.Errorf("目录指针落在了 section 里（catalog=%d > 首个 section=%d）：\n%s", catalogIdx, firstSection, got)
	}
}

// 缺陷 8（Important）：段头带行尾注释时不能重复声明同名段。
func TestTakeOverCodexNoDuplicateSectionWithTrailingComment(t *testing.T) {
	home := takeoverFixture(t,
		"model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n\n"+
			"[model_providers.aihub] # 已有\nbase_url = \"http://127.0.0.1:8790/v1\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	got := readCfg(t, home)
	if n := strings.Count(got, "[model_providers.aihub]"); n != 1 {
		t.Errorf("同名段声明 %d 次，应为 1：\n%s", n, got)
	}
}

// 缺陷 5（Important）：接管/还原写 config.toml 必须沿用原权限（0600 不许变 0644）。
func TestTakeOverRestorePreservesFileMode(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	path := filepath.Join(home, ".codex", "config.toml")

	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("接管后权限 = %v，应为 0600", st.Mode().Perm())
	}

	if _, err := restoreCodex(); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Mode().Perm() != 0600 {
		t.Errorf("还原后权限 = %v，应为 0600", st2.Mode().Perm())
	}
}

// 缺陷 6（Important）：网关 URL 只出现在注释/profile 里时，不许误判「已接管」而跳过备份。
func TestTakeOverCodexBacksUpEvenIfURLMentionedElsewhere(t *testing.T) {
	home := takeoverFixture(t,
		"model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n\n"+
			"[profiles.gw]\nopenai_base_url = \"http://127.0.0.1:8790/backend-api/codex\"\n")
	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "config.toml.bak-hubgo-*"))
	if len(matches) != 1 {
		t.Errorf("备份份数 = %d，应为 1（误判已接管会跳过备份，之后还原会丢配置）", len(matches))
	}
}

// 缺陷 12（Minor）：顶层原本没有 model 行时，文案不许声称改成了某个 model。
func TestTakeOverCodexMessageMatchesActualConfig(t *testing.T) {
	home := takeoverFixture(t, "model_provider = \"openai\"\n")
	msg, err := takeOverCodex()
	if err != nil {
		t.Fatal(err)
	}
	got := readCfg(t, home)
	claimsModel := strings.Contains(msg, "model="+aggDefaultModel)
	hasModel := strings.Contains(got, `model = "`+aggDefaultModel+`"`)
	if claimsModel && !hasModel {
		t.Errorf("文案声称 model=%s，但配置里没有该行：\n%s", aggDefaultModel, got)
	}
}

// 缺陷 13（Minor）：重复接管不许累积重复的 # aihub: 注释行与 openai_base_url。
func TestTakeOverCodexNoDuplicateCommentLines(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	for i := 0; i < 3; i++ {
		if _, err := takeOverCodex(); err != nil {
			t.Fatalf("第 %d 次接管失败：%v", i+1, err)
		}
	}
	got := readCfg(t, home)
	if n := strings.Count(got, "# aihub: Codex 的 responses 经统一网关转发"); n != 1 {
		t.Errorf("# aihub: 注释出现 %d 次，应为 1：\n%s", n, got)
	}
	// 注意注释行里也含 openai_base_url 字样，这里只数真正的赋值行
	if n := strings.Count(got, "openai_base_url = \""); n != 1 {
		t.Errorf("openai_base_url 赋值行出现 %d 次，应为 1：\n%s", n, got)
	}
}

func truncateForLog(s string) string {
	if len(s) <= 800 {
		return s
	}
	return s[:800] + "…"
}

// 缺陷 7（Important）：还原必须用「本次接管留的备份」，不能按时间戳拿到一个月前的旧备份。
func TestRestoreCodexPrefersTakeoverBackup(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	dir := filepath.Join(home, ".codex")

	// 造一个「一个月前的旧备份」：内容明显不同，若被误用会还原成它
	old := filepath.Join(dir, "config.toml.bak-hubgo-20261015-143405")
	if err := os.WriteFile(old, []byte("model = \"stale-month-old\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreCodex(); err != nil {
		t.Fatal(err)
	}
	got := readCfg(t, home)
	if strings.Contains(got, "stale-month-old") {
		t.Errorf("还原用了一个月前的旧备份：\n%s", got)
	}
	if !strings.Contains(got, `model = "gpt-5.2"`) {
		t.Errorf("还原没回到接管前的内容：\n%s", got)
	}
}

// 缺陷 9（Important）：网关 URL 只出现在 profile 段时，状态不许报「已接管」。
func TestApiClientsStatusIgnoresProfileBaseURL(t *testing.T) {
	takeoverFixture(t,
		"model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n\n"+
			"[profiles.gw]\nopenai_base_url = \"http://127.0.0.1:8790/backend-api/codex\"\n")
	rec := httptest.NewRecorder()
	(&Hub{}).apiClientsStatus(rec, httptest.NewRequest(http.MethodGet, "/api/clients/status", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["codex_taken_over"] != false {
		t.Errorf("profile 里有网关 URL 时 codex_taken_over = %v，应为 false（界面不许说谎）", out["codex_taken_over"])
	}
}

// 缺陷 3（Critical）：文本手术产出非法 TOML 时必须拒绝写入并还原。
func TestWriteFileCheckedRejectsBadTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := []byte("model = \"ok\"\n")
	if err := os.WriteFile(path, orig, 0600); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dir, "config.toml.bak-hubgo-20260101-000000")
	if err := os.WriteFile(bak, orig, 0600); err != nil {
		t.Fatal(err)
	}

	// 非法 TOML：同一个键赋值两次
	bad := []byte("model = \"a\"\nmodel = \"b\"\n")
	err := writeFileChecked(path, bad, 0600, bak)
	if err == nil {
		t.Fatal("非法 TOML 必须被拒绝")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(orig) {
		t.Errorf("拒绝后应保持原内容，实际：\n%s", got)
	}
	if _, serr := os.Stat(path + ".tmp"); serr == nil {
		t.Error("拒绝后不该残留 .tmp")
	}

	// 合法 TOML：正常写入
	good := []byte("model = \"c\"\n")
	if err := writeFileChecked(path, good, 0600, bak); err != nil {
		t.Fatalf("合法 TOML 应写入成功：%v", err)
	}
	got2, _ := os.ReadFile(path)
	if string(got2) != string(good) {
		t.Errorf("写入内容不符：%s", got2)
	}
}
