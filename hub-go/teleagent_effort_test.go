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

// 2026-11-03：X-TeleAI-Client-Type 必须真的发出去。
// 之前只定义了 teleagentClientType()，全仓库零调用点 —— 官网「调用端」列因此一直是空的。
func TestTeleagentSignHeadersIncludeClientType(t *testing.T) {
	tok := "aaa.bbb.ccc"
	h, err := teleagentSignHeadersAt(tok, "dev123", "install-1", "POST",
		"/superCowork/sapi/api/v1/chat/completions", "3.0.0-rc.1", []byte(`{}`), "1700000000", "nonce-1")
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}
	if h["X-TeleAI-Client-Type"] != "desktop" {
		t.Errorf("调用端标识应为 desktop，实际 %q", h["X-TeleAI-Client-Type"])
	}
	// env 可覆盖（上游若改枚举值）
	t.Setenv("AIHUB_TELEAGENT_CLIENT_TYPE", "app")
	if got := teleagentClientType(); got != "app" {
		t.Errorf("env 应可覆盖，实际 %q", got)
	}
}
