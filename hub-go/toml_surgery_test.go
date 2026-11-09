package main

// toml_surgery.go 的单元测试。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTomlSectionName(t *testing.T) {
	cases := []struct{ line, want string }{
		{"[model_providers.aihub]", "model_providers.aihub"},
		{"[model_providers.aihub] # 已有", "model_providers.aihub"}, // 行尾注释（审查 Important 8）
		{"  [profiles.official]  ", "profiles.official"},
		{"[[profiles]]", "profiles"},
		// 数组元素行不是 section 头（审查 Minor 14）：] 后面还有逗号
		{`  [ "a", "b" ],`, ""},
		{"model = \"gpt-5.2\"", ""},
		{"# [fake]", ""},
		{"", ""},
		{"[unclosed", ""},
	}
	for _, c := range cases {
		if got := tomlSectionName(c.line); got != c.want {
			t.Errorf("tomlSectionName(%q) = %q，应为 %q", c.line, got, c.want)
		}
	}
}

func TestTomlTopKey(t *testing.T) {
	cases := []struct {
		line    string
		wantKey string
		wantOK  bool
	}{
		{`model = "gpt-5.2"`, "model", true},
		{`model_context_window = 1000000`, "model_context_window", true},
		{`  openai_base_url = "http://x"`, "openai_base_url", true},
		{`# model = "x"`, "", false},
		{`[section]`, "", false},
		{`not a kv line`, "", false},
		{`key with spaces = 1`, "", false},
		{"", "", false},
	}
	for _, c := range cases {
		k, _, ok := tomlTopKey(c.line)
		if ok != c.wantOK || (ok && k != c.wantKey) {
			t.Errorf("tomlTopKey(%q) = (%q, %v)，应为 (%q, %v)", c.line, k, ok, c.wantKey, c.wantOK)
		}
	}
}

// 顶层键判定必须只看顶层，不能被 section 里的同名键影响（审查 Important 6/9）。
func TestTopLevelHasKeyIgnoresSections(t *testing.T) {
	src := "model = \"a\"\n\n[profiles.x]\nopenai_base_url = \"http://y\"\n"
	if !topLevelHasKey(src, "model") {
		t.Error("顶层应有 model")
	}
	if topLevelHasKey(src, "openai_base_url") {
		t.Error("openai_base_url 只在 section 里，不该算顶层有")
	}
	if got := topLevelValue(src, "model"); got != "a" {
		t.Errorf("topLevelValue(model) = %q，应为 a（函数会去掉引号）", got)
	}
	if got := topLevelValue(src, "openai_base_url"); got != "" {
		t.Errorf("topLevelValue(openai_base_url) 应为空（只在 section 里），实际 %q", got)
	}
}

func TestTomlValid(t *testing.T) {
	if ok, detail := tomlValid([]byte("model = \"x\"\n[a]\nk = 1\n")); !ok {
		t.Errorf("合法 TOML 被判非法：%s", detail)
	}
	// 重复键是典型非法（审查 Critical 2 的产物）
	if ok, _ := tomlValid([]byte("model = \"a\"\nmodel = \"b\"\n")); ok {
		t.Error("重复键应判非法")
	}
	// 同名段重复声明也是非法（审查 Important 8 的产物）
	if ok, _ := tomlValid([]byte("[a]\nk = 1\n[a]\nj = 2\n")); ok {
		t.Error("重复段应判非法")
	}
}

// writeFileChecked 校验失败且给了备份时，必须从备份还原。
func TestWriteFileCheckedRestoresFromBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	bak := filepath.Join(dir, "config.toml.bak-hubgo-20260101-000000")
	orig := []byte("model = \"good\"\n")
	if err := os.WriteFile(bak, orig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = \"broken\""), 0600); err != nil {
		t.Fatal(err)
	}
	err := writeFileChecked(path, []byte("model = \"a\"\nmodel = \"b\"\n"), 0600, bak)
	if err == nil {
		t.Fatal("非法 TOML 应报错")
	}
	if te, ok := err.(*tomlCheckError); !ok || te.restored == "" {
		t.Errorf("错误应带「已从备份还原」信息，实际 %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(orig) {
		t.Errorf("没从备份还原：%s", got)
	}
}

// 备份 marker：接管写 marker，还原读 marker 优先（审查 Important 7）。
func TestTakeoverMarkerDrivesPreferredBackup(t *testing.T) {
	home := takeoverFixture(t, "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n")
	cfg := filepath.Join(home, ".codex", "config.toml")

	// 先造一个「很旧」的备份，模拟本机 15 个旧备份的情况
	oldBak := cfg + ".bak-hubgo-20200101-000000"
	if err := os.WriteFile(oldBak, []byte("model = \"ancient\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := takeOverCodex(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(takeoverMarkerPath(cfg)); err != nil {
		t.Fatalf("接管后应写 marker：%v", err)
	}
	got := preferredBackup(cfg)
	if got == "" || got == oldBak {
		t.Errorf("preferredBackup = %q，应指向本次接管的备份而不是一个月前那个", got)
	}

	if _, err := restoreCodex(); err != nil {
		t.Fatal(err)
	}
	// 还原后 marker 应被清掉
	if _, err := os.Stat(takeoverMarkerPath(cfg)); err == nil {
		t.Error("还原后 marker 应删除")
	}
	b, _ := os.ReadFile(cfg)
	if string(b) != "model = \"gpt-5.2\"\nmodel_provider = \"openai\"\n" {
		t.Errorf("没还原到接管前原文：\n%s", b)
	}
}
