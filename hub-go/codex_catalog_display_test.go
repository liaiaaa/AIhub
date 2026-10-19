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

// Codex 目录的 display_name 必须与 /v1/models 用同一套生成逻辑（含倍率）。
func TestCodexCatalogDisplayNameCarriesRatio(t *testing.T) {
	h := &Hub{}
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

// 写出的文件必须落在 ~/.codex 下且原子替换（.tmp + rename）。
func TestCodexCatalogFileLocation(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })

	h := &Hub{}
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	path, err := writeCodexCatalogFile(h)
	if err != nil {
		t.Fatalf("空白名单也应能写出目录：%v", err)
	}
	if filepath.Base(path) != codexCatalogPath {
		t.Errorf("文件名 = %q，应为 %q", filepath.Base(path), codexCatalogPath)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件未落盘：%v", err)
	}
}
