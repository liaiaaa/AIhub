package main

// 2026-10-19：Codex 桌面版目录文件里的 display_name 要带倍率与上下文。
//
// 背景：原来 writeCodexCatalogFile 直接用 am.Display（聚合白名单里手写的静态
// 字符串），只有「DeepSeek V4 Flash（国内）」，没有倍率。而同一时刻
// /v1/models 返回的是「DeepSeek V4 Flash（国内） · 0.17x · 上下文 1M」。
// 两个客户端看到的模型信息不一致，用户在 Codex 里看不到计价。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateUserHome 把 homeJoin 的落盘位置指到临时目录。
//
// 2026-10-27 事故根因之一：homeJoin 原来恒用 os.UserHomeDir()，测试里设
// hubRoot = t.TempDir() 对它无效，于是 TestCodexCatalogFileLocation 把用户
// 真实的 ~/.codex/aihub-model-catalog.json 写成了 {"models": []}，Codex 桌面版
// 报 "must contain at least one model" 打不开。测试一律先调这个函数。
func isolateUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	old := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = old })
	return home
}

// Codex 目录的 display_name 必须与 /v1/models 用同一套生成逻辑（含倍率）。
func TestCodexCatalogDisplayNameCarriesRatio(t *testing.T) {
	h := &Hub{}
	home := isolateUserHome(t)
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })

	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{
		{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash",
			Display: "DeepSeek V4 Flash（国内）"},
	}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	path, err := writeCodexCatalogFile(h)
	if err != nil {
		t.Fatalf("写 Codex 目录失败：%v", err)
	}
	if want := filepath.Join(home, ".codex", codexCatalogPath); path != want {
		t.Fatalf("落盘路径 = %q，应为 %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Models) != 1 {
		t.Fatalf("应有 1 个模型，实际 %d", len(doc.Models))
	}
	dn, _ := doc.Models[0]["display_name"].(string)
	if dn == "" {
		t.Fatal("display_name 为空")
	}
	// 必须带倍率（国内 deepseek-v4-flash 静态表是 0.06x）
	if !strings.Contains(dn, "x") {
		t.Errorf("display_name 缺倍率：%q", dn)
	}
	// 必须带上下文
	if !strings.Contains(dn, "上下文") {
		t.Errorf("display_name 缺上下文：%q", dn)
	}
	// 仍要保留线路归属，否则 Codex 里分不清是哪条线
	if !strings.Contains(dn, "国内") {
		t.Errorf("display_name 缺线路归属：%q", dn)
	}
}

// 目录刷新后要主动重写 Codex 目录文件（Codex 只在启动时读一次）。
func TestWBCatalogRefreshTriggersCodexCatalog(t *testing.T) {
	// 这条是行为约束而非端到端：确认 refreshWBCatalog 的成功分支里
	// 确实调用了 codexCatalogRefresh。用源码文本断言，避免真去连上游。
	b, err := os.ReadFile("wb_config.go")
	if err != nil {
		t.Skipf("读不到源码：%v", err)
	}
	s := string(b)
	// 取 refreshWBCatalog 函数体
	start := strings.Index(s, "func (h *Hub) refreshWBCatalog")
	if start < 0 {
		t.Fatal("找不到 refreshWBCatalog")
	}
	end := strings.Index(s[start:], "\n}")
	if end < 0 {
		t.Fatal("函数体不完整")
	}
	body := s[start : start+end]
	if !strings.Contains(body, "codexCatalogRefresh()") {
		t.Error("refreshWBCatalog 成功分支没有触发 codexCatalogRefresh —— " +
			"倍率变了 Codex 目录不会更新")
	}
}

// 写出的文件必须落在 <home>/.codex 下且原子替换（.tmp + rename）。
func TestCodexCatalogFileLocation(t *testing.T) {
	home := isolateUserHome(t)
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })

	h := &Hub{}
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{
		{Line: "intl", Slug: "intl.deepseek-v4.1-flash", Backend: "deepseek-v4.1-flash",
			Display: "DeepSeek V4.1 Flash（国际）"},
		{Line: "qoder", Slug: "qoder.DeepSeek-Flash", Backend: "DeepSeek-Flash",
			Display: "DeepSeek Flash（Qoder）"},
	}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	path, err := writeCodexCatalogFile(h)
	if err != nil {
		t.Fatalf("写目录失败：%v", err)
	}
	if want := filepath.Join(home, ".codex", codexCatalogPath); path != want {
		t.Errorf("落盘路径 = %q，应为 %q", path, want)
	}
	if filepath.Base(path) != codexCatalogPath {
		t.Errorf("文件名 = %q，应为 %q", filepath.Base(path), codexCatalogPath)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件未落盘：%v", err)
	}
	// 原子替换：.tmp 不能留下
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp 残留：%v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Models) != 2 {
		t.Fatalf("应有 2 个模型，实际 %d", len(doc.Models))
	}
}

// 白名单 0 个模型时绝不覆盖已有目录文件（2026-10-27 Codex 打不开的事故）。
func TestCodexCatalogSkipsEmptyWhitelist(t *testing.T) {
	home := isolateUserHome(t)
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	path := filepath.Join(home, ".codex", codexCatalogPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte(`{"models":[{"slug":"keep-me"}]}`)
	if err := os.WriteFile(path, sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writeCodexCatalogFile(&Hub{}); err == nil {
		t.Fatal("白名单为空时应返回错误，而不是静默写空表")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("已有目录文件被改写了：%q", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime 变了：%v → %v", before.ModTime(), after.ModTime())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp 不应被创建：%v", err)
	}
}
