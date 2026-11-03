package main

// 2026-11-03：图片能力按模型判定（用户实测：teleagent.chat-flagship 发图 →
// 上游 400「messages.content.type 参数非法，取值范围 ['text']」）。

import (
	"strings"
	"testing"
)

func TestModelSupportsImagePerModel(t *testing.T) {
	cases := []struct {
		line, model string
		want        bool
	}{
		{"teleagent", "chat-flash", true},
		{"teleagent", "chat-pro", true},
		{"teleagent", "chat-lite", true},
		{"teleagent", "chat-flagship", false}, // 实测纯文本
		{"teleagent", "chat-nano", false},     // 实测 telechat-29b 非多模态
		{"dom", "deepseek-v4-flash", true},
		{"qoder", "DeepSeek-Flash", true},
		{"trae", "kimi-k3", true},
		{"oc", "anything", false}, // 线路本身不宣告图片
		{"mimo", "v2", false},
	}
	for _, c := range cases {
		if got := modelSupportsImage(c.line, c.model); got != c.want {
			t.Errorf("modelSupportsImage(%q,%q) = %v，期望 %v", c.line, c.model, got, c.want)
		}
	}
}

// DSH 清单里，纯文本模型的 input 不能写 image —— 否则 DSH 会真的发图，然后 400。
func TestBuildDshModelsRespectsPerModelImage(t *testing.T) {
	withAggList(t, []aggModel{
		{Slug: "teleagent.chat-flagship", Line: "teleagent", Backend: "chat-flagship"},
		{Slug: "teleagent.chat-flash", Line: "teleagent", Backend: "chat-flash"},
	})
	models := buildDshModels(&Hub{})
	got := map[string][]string{}
	for _, m := range models {
		got[m.ID] = m.Input
	}
	if len(got["teleagent.chat-flagship"]) != 1 || got["teleagent.chat-flagship"][0] != "text" {
		t.Errorf("chat-flagship 应为纯文本，实际 %v", got["teleagent.chat-flagship"])
	}
	if len(got["teleagent.chat-flash"]) != 2 || got["teleagent.chat-flash"][1] != "image" {
		t.Errorf("chat-flash 应支持图片，实际 %v", got["teleagent.chat-flash"])
	}
	y := dshModelsYAML(models, "  ")
	if strings.Count(y, "- image") != 1 {
		t.Errorf("YAML 里应只有 1 个 image 条目，实际 %d", strings.Count(y, "- image"))
	}
}
