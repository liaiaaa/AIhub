package main

// 2026-11-04 实测：TeleAgent 的模型都是「先思考再答」，max_tokens 太小会把预算全花在
// reasoning 上，正文 0 字、finish_reason=length，客户端看到空回答就开始无限重试
// （用户那个会话 20:04 连续 5 次 2 秒断流就是这么来的）。
//   max_tokens=60  → 28.9s，reasoning 110 字，正文 0 字
//   max_tokens=300 → 1.5s，正文 77 字
// 所以转发前给个下限。

import "testing"

func TestTeleagentMaxTokensRead(t *testing.T) {
	cases := []struct {
		payload map[string]any
		want    int
	}{
		{map[string]any{"max_tokens": float64(60)}, 60},
		{map[string]any{"max_completion_tokens": float64(128)}, 128},
		{map[string]any{"max_tokens": 256}, 256},
		{map[string]any{}, 0},
	}
	for _, c := range cases {
		if got := teleagentMaxTokens(c.payload); got != c.want {
			t.Errorf("teleagentMaxTokens(%v) = %d，期望 %d", c.payload, got, c.want)
		}
	}
}

func TestTeleagentMinTokens(t *testing.T) {
	t.Setenv("AIHUB_TELEAGENT_MIN_TOKENS", "")
	if got := teleagentMinTokens(); got != 1024 {
		t.Errorf("默认下限应为 1024，实际 %d", got)
	}
	t.Setenv("AIHUB_TELEAGENT_MIN_TOKENS", "2048")
	if got := teleagentMinTokens(); got != 2048 {
		t.Errorf("env 应生效，实际 %d", got)
	}
	t.Setenv("AIHUB_TELEAGENT_MIN_TOKENS", "bad")
	if got := teleagentMinTokens(); got != 1024 {
		t.Errorf("非法 env 应回默认，实际 %d", got)
	}
}
