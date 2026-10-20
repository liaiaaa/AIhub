package main

// 2026-10-20：qoder 网关不接受 assistant/tool 消息的 null content。
//
// 用户报「DeepSeek-Flash 一直 provider_error 根本没法用」。上游返回的错误是
//   "Messages with role 'tool' must be a response to a preceding message with
//    'tool_calls'"
// 这个文案把排查引向 tool 配对，但实测 tool 配对完全正确。真正的触发条件是
// dsh 发带 tool_calls 的 assistant 消息时 content 为 null（OpenAI 规范允许）。
//
// 二分定位证据（同一份真实 dump，唯一改动是 content）：
//   完整 27 条           → ✗ provider_error
//   content=None → ""    → ✅ 通过
//   前 21 条（含 None）   → ✗ provider_error
//   前 21 条（None→""）  → ✅ 通过

import (
	"encoding/json"
	"testing"
)

// assistant 带 tool_calls 且 content=null → 必须补成 ""
func TestNormalizeNullContentAssistant(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{
			"role":    "assistant",
			"content": nil, // ← dsh 的形态
			"tool_calls": []any{map[string]any{
				"id": "c1", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": "{}"},
			}},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": nil},
		map[string]any{"role": "user", "content": "继续"},
	}
	out := qoderNormalizeMessages(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("消息数不该变：%d → %d", len(msgs), len(out))
	}
	for i, raw := range out {
		m := raw.(map[string]any)
		role := m["role"]
		if (role == "assistant" || role == "tool") && m["content"] == nil {
			t.Fatalf("#%d %v 的 content 仍是 null，上游会整包拒绝", i, role)
		}
	}
	// tool_calls 必须原样保留
	a := out[2].(map[string]any)
	if a["tool_calls"] == nil {
		t.Fatal("tool_calls 被弄丢了")
	}
}

// content 为 "" 或正常字符串时不能被改坏
func TestNormalizeKeepsNonNilContent(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{}},
		map[string]any{"role": "assistant", "content": "有内容"},
		map[string]any{"role": "tool", "content": "结果"},
	}
	out := qoderNormalizeMessages(msgs)
	if len(out) != 3 {
		t.Fatalf("不该丢消息：%d", len(out))
	}
	if out[1].(map[string]any)["content"] != "有内容" {
		t.Error("正常 content 被改动了")
	}
	if out[2].(map[string]any)["content"] != "结果" {
		t.Error("tool content 被改动了")
	}
}

// 回归：用真实失败样本的形态跑一遍，确认 null 都被消掉。
func TestNormalizeRealFailureShape(t *testing.T) {
	// 复刻真实 dump 的关键形态：#19 assistant content=null + tool_calls，
	// 后面跟 tool 响应（这是唯一触发点，前面 20 条都正常）
	raw := `[
	  {"role":"system","content":"sys"},
	  {"role":"user","content":"a"},
	  {"role":"assistant","content":"ok"},
	  {"role":"user","content":"b"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_x","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	  {"role":"tool","tool_call_id":"call_x","content":"输出"},
	  {"role":"assistant","content":"done"}
	]`
	var msgs []any
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatal(err)
	}
	out := qoderNormalizeMessages(msgs)
	for i, r := range out {
		m := r.(map[string]any)
		if m["content"] == nil {
			t.Fatalf("#%d %v content 仍为 null", i, m["role"])
		}
	}
	if len(out) != 7 {
		t.Fatalf("消息数不该变：7 → %d", len(out))
	}
}

// 2026-10-20 第二个 bug：assistant 带 tool_calls 但后面没有 tool 响应，
// 上游同样整包拒绝。真实请求里很常见（历史截断 / 工具执行到一半）。
//
// 二分证据（真实 dump 28 条，末尾 #27 是孤立 tool_calls）：
//   完整 28 条     → ✗ provider_error
//   去掉最后一条    → ✅ 通过
//   补上 tool 响应  → ✅ 通过
func TestFillDanglingToolCalls(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "grep", "arguments": "{}"}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "有响应"},
		// c2 没有响应
		map[string]any{"role": "user", "content": "继续"},
	}
	out := qoderFillDanglingToolCalls(msgs)
	if len(out) != len(msgs)+1 {
		t.Fatalf("应补 1 条占位 tool，实际 %d → %d", len(msgs), len(out))
	}
	// 补的那条要紧跟 assistant（索引 2）
	added, ok := out[2].(map[string]any)
	if !ok || added["role"] != "tool" || added["tool_call_id"] != "c2" {
		t.Fatalf("占位消息位置/内容不对：%v", out[2])
	}
	if added["content"] == "" || added["content"] == nil {
		t.Error("占位内容不能为空（上游可能同样拒绝）")
	}
	// 原本已响应的 c1 不能再补一条
	n := 0
	for _, r := range out {
		if m, ok := r.(map[string]any); ok && m["role"] == "tool" && m["tool_call_id"] == "c1" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("c1 应只有 1 条 tool 响应，实际 %d", n)
	}
}

// 配对完整时不能多补任何东西（幂等）
func TestFillDanglingToolCallsNoop(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "bash"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
	}
	out := qoderFillDanglingToolCalls(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("配对完整时不该补：%d → %d", len(msgs), len(out))
	}
}

// 端到端：normalize 之后，真实失败形态（null content + 孤立 tool_calls）必须全修好
func TestNormalizeFixesBothRealFailureModes(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "a"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "bash"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "输出"},
		// 末尾孤立 tool_calls（第二个 bug）
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "grep"}},
		}},
	}
	out := qoderNormalizeMessages(msgs)
	// 1) 不能有 null content
	for i, r := range out {
		if m := r.(map[string]any); m["content"] == nil {
			t.Fatalf("#%d %v content 仍为 null", i, m["role"])
		}
	}
	// 2) 每个 tool_call 都要有响应
	answered := map[string]bool{}
	for _, r := range out {
		if m := r.(map[string]any); m["role"] == "tool" {
			answered[m["tool_call_id"].(string)] = true
		}
	}
	for _, r := range out {
		m := r.(map[string]any)
		if tcs, ok := m["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				id := tc.(map[string]any)["id"].(string)
				if !answered[id] {
					t.Fatalf("tool_call %s 没有响应，上游会拒绝", id)
				}
			}
		}
	}
}
