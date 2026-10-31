package main

// 2026-10-27：agg /v1/models 的「OpenAI 形态」日志一直报 0 个模型。
//
// 根因：aggOpenAIModels() 的 data 是 Go 原生 []map[string]any，
// 而 asSlice() 只认 JSON 解析出来的 []any → 断言必定失败 → 日志恒为 0。
// 实测 curl 拿到 14 条，日志同时打「0 个模型」，排查时被带偏过一轮。

import "testing"

func TestAggOpenAIModelCount(t *testing.T) {
	// Go 原生构造（真实路径）：必须数对
	native := map[string]any{"data": []map[string]any{
		{"id": "a"}, {"id": "b"}, {"id": "c"},
	}}
	if got := aggOpenAIModelCount(native); got != 3 {
		t.Fatalf("Go 原生 []map[string]any 应数到 3，实际 %d", got)
	}

	// JSON 解析产物（如测试桩）：也要兼容
	parsed := map[string]any{"data": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}
	if got := aggOpenAIModelCount(parsed); got != 2 {
		t.Fatalf("[]any 应数到 2，实际 %d", got)
	}

	// 空表 / 字段缺失 / 类型不对：0，不能 panic
	for _, out := range []map[string]any{
		{"data": []map[string]any{}},
		{},
		{"data": "oops"},
		{"data": nil},
	} {
		if got := aggOpenAIModelCount(out); got != 0 {
			t.Fatalf("%v 应数到 0，实际 %d", out, got)
		}
	}
}
