package main

// 2026-10-19 用户报「qoder 的 DeepSeek-Flash 半天不输出」。
//
// 根因不是模型路由（dfmodel 解析正确、非流式正常、计费一致），而是
// qoderStreamToolBugModels 里的预判式降级：只要「流式 + 历史含工具消息」
// 就强制把上游请求改成 stream=false，聚合完整响应后再一次性回灌 SSE。
// 实测同一请求：真流式首字节 0.207s / 总 1.138s；降级后首字节 0.627s 且
// 一次性吐出。多轮工具调用场景下就是「半天不输出」。
//
// 2026-10-19 探针实测上游已修复：stream=true + 工具历史（dfmodel/思考档 max）
// 上游 HTTP 200、首字节 0.38s、23 个增量帧、tool_calls 帧完整（id/name/
// arguments/finish_reason 齐全）、unwrap 零报错。故默认改为真流式，
// 预判式降级降级为 QODER_AUTO_NOSTREAM=1 的应急开关。

import (
	"os"
	"testing"
)

// 默认必须是真流式（不预判降级）——这是用户体感问题的直接开关。
func TestQoderAutoNoStreamDefaultsOff(t *testing.T) {
	old, had := os.LookupEnv("QODER_AUTO_NOSTREAM")
	defer func() {
		if had {
			os.Setenv("QODER_AUTO_NOSTREAM", old)
		} else {
			os.Unsetenv("QODER_AUTO_NOSTREAM")
		}
	}()

	os.Unsetenv("QODER_AUTO_NOSTREAM")
	if qoderAutoNoStreamEnabled() {
		t.Fatal("未设 QODER_AUTO_NOSTREAM 时应为真流式（预判式降级必须默认关闭）")
	}

	os.Setenv("QODER_AUTO_NOSTREAM", "0")
	if qoderAutoNoStreamEnabled() {
		t.Fatal("QODER_AUTO_NOSTREAM=0 时应为真流式")
	}

	os.Setenv("QODER_AUTO_NOSTREAM", "1")
	if !qoderAutoNoStreamEnabled() {
		t.Fatal("QODER_AUTO_NOSTREAM=1 时应恢复预判式降级（应急回退）")
	}
}

// 降级名单本身保留（应急回退要用），但默认路径不该读它做预判。
func TestQoderStreamToolBugModelsStillAvailable(t *testing.T) {
	for _, k := range []string{"dfmodel", "dmodel", "kmodel"} {
		if !qoderStreamToolBugModels[k] {
			t.Fatalf("应急回退名单里应保留 %q", k)
		}
	}
}

// 工具历史识别：role=tool 与 assistant.tool_calls 都要认。
func TestQoderHistoryHasToolMsgs(t *testing.T) {
	noTool := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}
	if qoderHistoryHasToolMsgs(noTool) {
		t.Fatal("没有工具消息时不该判为有")
	}
	withToolRole := []any{map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"}}
	if !qoderHistoryHasToolMsgs(withToolRole) {
		t.Fatal("role=tool 应判为有工具历史")
	}
	withCalls := []any{map[string]any{"role": "assistant", "content": "",
		"tool_calls": []any{map[string]any{"id": "c1"}}}}
	if !qoderHistoryHasToolMsgs(withCalls) {
		t.Fatal("assistant.tool_calls 应判为有工具历史")
	}
}
