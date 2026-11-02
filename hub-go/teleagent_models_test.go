package main

// 2026-11-02：TeleAgent 模型清单的权威来源是应用自己的 model-catalog.json。
// 用户问「这几个模型分别对应什么」，答案就钉在这张表里，避免以后有人凭印象改名。

import "testing"

func TestTeleagentModelMapping(t *testing.T) {
	// id → 应用内显示名 / 上下文 / 输出上限
	want := map[string]struct {
		name    string
		context int
		maxOut  int
	}{
		"chat-pro":      {"智能", 1000000, 65536},
		"chat-flagship": {"旗舰", 1000000, 65536},
		"chat-flash":    {"极速", 1000000, 65536},
		"chat-lite":     {"轻量", 100000, 16384},
		"chat-nano":     {"轻速", 100000, 16384},
	}
	got := map[string]struct {
		name    string
		context int
		maxOut  int
	}{}
	for _, m := range teleagentModels {
		if m.Disabled {
			continue
		}
		got[m.ID] = struct {
			name    string
			context int
			maxOut  int
		}{m.Display, m.Context, m.MaxOut}
	}
	if len(got) != len(want) {
		t.Fatalf("对外模型数 = %d，期望 %d", len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("缺模型 %s", id)
			continue
		}
		if g != w {
			t.Errorf("%s = %+v，期望 %+v", id, g, w)
		}
	}
	// tele-safety-answer 必须留在名单里但标记 disabled（应用自己 blacklist 它）
	found := false
	for _, m := range teleagentModels {
		if m.ID == "tele-safety-answer" {
			found = true
			if !m.Disabled {
				t.Error("tele-safety-answer 应标记 Disabled")
			}
		}
	}
	if !found {
		t.Error("tele-safety-answer 应从名单里显式排除，而不是凭空消失")
	}
}

func TestTeleagentRatios(t *testing.T) {
	want := map[string]float64{"chat-pro": 0.6, "chat-flagship": 1.0, "chat-flash": 0.2}
	for id, w := range want {
		if got := modelRatios["teleagent"][id]; got != w {
			t.Errorf("倍率 %s = %v，期望 %v", id, got, w)
		}
	}
}
