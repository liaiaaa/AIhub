package main

import (
	"strings"
	"testing"
)

// 接管状态必须按 openai_base_url 判定（A′），不能再看 model_provider。
func TestConsoleCodexTakeoverUsesBaseURL(t *testing.T) {
	src := consoleSource(t)
	if !strings.Contains(src, "clients.codex_taken_over") {
		t.Error("总览/设置页应读 clients.codex_taken_over")
	}
	if !strings.Contains(src, "clients.codex_base_url") {
		t.Error("设置页应展示 clients.codex_base_url")
	}
	if strings.Contains(src, "clients.codex_provider === 'aihub'") {
		t.Error("旧的 provider==aihub 判定必须去掉（A′ 下顶层不再写 model_provider）")
	}
	if strings.Contains(src, "d.codex_provider === 'aihub'") {
		t.Error("设置页旧判定也要去掉")
	}
}

// 文案要说明新机制，避免用户按旧文档理解。
func TestConsoleCodexTakeoverCopy(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{"openai_base_url", "混合目录", "官方登录身份"} {
		if !strings.Contains(src, want) {
			t.Errorf("接管文案应包含 %q", want)
		}
	}
}
