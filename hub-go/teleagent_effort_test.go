package main

// 2026-11-03：teleagent 档位归一化（上游只认 low/high/max，传 minimal 会 400）。

import "testing"

func TestTeleagentNormalizeEffort(t *testing.T) {
	cases := []struct {
		in   any
		want any
		gone bool
	}{
		{"minimal", "low", false}, // 上游不允许关思考 → 折到最轻的 low
		{"medium", "high", false}, // 没有 medium → high
		{"xhigh", "max", false},   // 没有 xhigh → max
		{"max", "max", false},     // 合法值原样
		{"LOW", "low", false},     // 大小写不敏感
		{"turbo", nil, true},      // 认不出 → 删掉，交给上游默认
		{"", nil, true},           // 空值 → 删掉
	}
	for _, c := range cases {
		m := map[string]any{"reasoning_effort": c.in}
		got := teleagentNormalizeEffort(m)
		if c.gone {
			if _, ok := got["reasoning_effort"]; ok {
				t.Errorf("%v 应被删除，实际 %v", c.in, got["reasoning_effort"])
			}
			continue
		}
		if got["reasoning_effort"] != c.want {
			t.Errorf("%v 应归一化为 %v，实际 %v", c.in, c.want, got["reasoning_effort"])
		}
	}
	// 没有该字段就不动
	m := map[string]any{"model": "chat-pro"}
	teleagentNormalizeEffort(m)
	if _, ok := m["reasoning_effort"]; ok {
		t.Error("没有档位字段时不该写入")
	}
}

func TestTeleagentEffortRejectsMinimalUpstreamName(t *testing.T) {
	// 反向确认：归一化后绝不会把 minimal 发给上游
	m := teleagentNormalizeEffort(map[string]any{"reasoning_effort": "minimal"})
	if m["reasoning_effort"] == "minimal" {
		t.Error("minimal 绝不能透传给上游（会 400）")
	}
}
