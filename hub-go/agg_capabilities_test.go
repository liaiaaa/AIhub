package main

// 2026-11-03：/v1/models 的能力字段与 Codex 目录的 per-model 元数据。

import (
	"strings"
	"testing"
)

func withAggList(t *testing.T, list []aggModel) {
	t.Helper()
	aggMu.Lock()
	saved := aggList
	aggList = list
	aggMu.Unlock()
	t.Cleanup(func() {
		aggMu.Lock()
		aggList = saved
		aggMu.Unlock()
	})
}

func TestAggModelsCarryCapabilityFields(t *testing.T) {
	withAggList(t, []aggModel{
		{Slug: "intl.deepseek-v4.1-flash", Line: "intl", Backend: "deepseek-v4.1-flash", Display: "DeepSeek"},
		{Slug: "teleagent.chat-pro", Line: "teleagent", Backend: "chat-pro", Display: "chat-pro"},
		{Slug: "mimo.MiMo-V2-Flash", Line: "mimo", Backend: "MiMo-V2-Flash", Display: "MiMo"},
	})
	h := &Hub{}
	out := h.aggOpenAIModels()
	data, _ := out["data"].([]map[string]any)
	if len(data) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(data))
	}
	var intl, ta, mimo map[string]any
	for _, e := range data {
		switch e["id"] {
		case "intl.deepseek-v4.1-flash":
			intl = e
		case "teleagent.chat-pro":
			ta = e
		case "mimo.MiMo-V2-Flash":
			mimo = e
		}
	}
	if intl == nil || ta == nil || mimo == nil {
		t.Fatal("条目缺失")
	}
	// intl：有档位、可发图、声明 tools
	if intl["type"] != "model" {
		t.Errorf("intl 缺 type=model：%v", intl["type"])
	}
	if intl["reasoning"] != true {
		t.Errorf("intl 应声明 reasoning=true：%v", intl["reasoning"])
	}
	if _, ok := intl["supported_reasoning_levels"]; !ok {
		t.Error("intl 缺 supported_reasoning_levels")
	}
	m, _ := intl["modalities"].(map[string]any)
	in, _ := m["input"].([]string)
	if len(in) != 2 { // text + image
		t.Errorf("intl 应支持 text+image，实际 %v", in)
	}
	if sp, _ := intl["supported_parameters"].([]string); len(sp) != 1 || sp[0] != "tools" {
		t.Errorf("intl 应声明 tools，实际 %v", intl["supported_parameters"])
	}
	// teleagent：2026-11-03 实测支持 low/high/max → 应宣告档位
	if ta["reasoning"] != true {
		t.Errorf("teleagent 实测支持档位，应 reasoning=true：%v", ta["reasoning"])
	}
	if lv, _ := ta["reasoning_levels"].([]string); len(lv) != 3 {
		t.Errorf("teleagent 应为 3 档，实际 %v", ta["reasoning_levels"])
	}
	// mimo：拿不到登录数据、未实测 → 不宣告
	if mimo["reasoning"] != false {
		t.Errorf("mimo 未验证前不应宣告 reasoning：%v", mimo["reasoning"])
	}
	if _, ok := mimo["supported_reasoning_levels"]; ok {
		t.Error("mimo 不应有 supported_reasoning_levels")
	}
}

func TestCodexCatalogUsesPerModelWindow(t *testing.T) {
	withAggList(t, []aggModel{
		{Slug: "intl.deepseek-v4.1-flash", Line: "intl", Backend: "deepseek-v4.1-flash"},
		{Slug: "trae.deepseek-v4.1-flash", Line: "trae", Backend: "deepseek-v4.1-flash"},
	})
	h := &Hub{}
	out := h.aggCodexModels()
	models, _ := out["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(models))
	}
	for _, raw := range models {
		m := raw.(map[string]any)
		slug := m["slug"].(string)
		if m["context_window"].(int) <= 0 {
			t.Errorf("%s 的 context_window 应 >0，实际 %v", slug, m["context_window"])
		}
		if _, ok := m["auto_compact_token_limit"]; !ok {
			t.Errorf("%s 缺 auto_compact_token_limit", slug)
		}
		// priority 连续编号
		if m["priority"].(int) < 1 {
			t.Errorf("%s 的 priority 应 >=1", slug)
		}
	}
	// trae 模型的档位必须是 3 档（low/high/max），不是硬编码的 4 档
	var traeEntry map[string]any
	for _, raw := range models {
		if m := raw.(map[string]any); strings.HasPrefix(m["slug"].(string), "trae.") {
			traeEntry = m
		}
	}
	levels, _ := traeEntry["supported_reasoning_levels"].([]any)
	if len(levels) != 3 {
		t.Errorf("trae 应宣告 3 档，实际 %d：%v", len(levels), levels)
	}
	if traeEntry["default_reasoning_level"] != "max" {
		t.Errorf("trae 默认档应为 max，实际 %v", traeEntry["default_reasoning_level"])
	}
}
