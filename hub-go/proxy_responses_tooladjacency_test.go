package main

import (
	"strings"
	"testing"
)

// 上游 WorkBuddy 要求 assistant.tool_calls 与它的 tool 结果严格相邻，中间不能夹
// 任何其它角色，否则 400 code 11148 "tool calls and tool results do not match"。
// Codex 在带图的 function_call_output 之后会回放一条 <image_resize_notice> 的
// system 条目，并行工具调用时正好夹在中间，故 responsesInputToMessages 出口要重排。

func imgOutput(url string) []any {
	return []any{map[string]any{"type": "input_image", "image_url": url}}
}

func textItem(role, text string) map[string]any {
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func callItem(id string) map[string]any {
	return map[string]any{"type": "function_call", "name": "view_image", "call_id": id, "arguments": "{}"}
}

func outputItem(id string, parts []any) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": id, "output": parts}
}

const notice = "<image_resize_notice>\nImage 1 of 1 in the preceding tool output was resized.\n</image_resize_notice>"

// adjacencyViolations 数出「结果被其它消息插队」的工具调用组数量。
func adjacencyViolations(msgs []any) int {
	bad := 0
	for i, m := range msgs {
		waiting := assistantToolCallIDs(m)
		if len(waiting) == 0 {
			continue
		}
		for j := i + 1; j < len(msgs) && len(waiting) > 0; j++ {
			mm, _ := msgs[j].(map[string]any)
			if mm == nil {
				bad++
				break
			}
			id := toolResultCallID(mm)
			if id == "" {
				bad++ // 插队的非 tool 消息
				continue
			}
			if _, pending := waiting[id]; !pending {
				bad++ // 结果缺位或对不上
				continue
			}
			delete(waiting, id)
		}
		if len(waiting) > 0 {
			bad++
		}
	}
	return bad
}

func TestInterleavedNoticeIsMovedPastToolBlock(t *testing.T) {
	raw := map[string]any{"input": []any{
		textItem("developer", "You are OpenAI Codex."),
		textItem("user", "北京和上海的天气"),
		callItem("call_00_a"),
		callItem("call_01_b"),
		outputItem("call_00_a", imgOutput("data:image/png;base64,AAA")),
		textItem("system", notice),
		outputItem("call_01_b", imgOutput("data:image/png;base64,BBB")),
		textItem("system", notice),
		textItem("user", "继续"),
	}}
	msgs := responsesInputToMessages(raw)

	if n := adjacencyViolations(msgs); n != 0 {
		t.Fatalf("工具结果仍有插队，violations=%d roles=%s", n, rolesOf(msgs))
	}

	// 具体形状：assistant(2 calls) → tool a → tool b → 两条 notice → user
	// （developer 角色在本层保持原样，developer→system 的映射发生在 handleChat）
	var seq []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if id := toolResultCallID(mm); id != "" {
			seq = append(seq, "tool:"+id)
			continue
		}
		c, _ := mm["content"].(string)
		if mm["role"] == "assistant" {
			seq = append(seq, "assistant")
			continue
		}
		if strings.Contains(c, "image_resize_notice") {
			seq = append(seq, "notice")
			continue
		}
		seq = append(seq, string(mm["role"].(string)))
	}
	want := strings.Join([]string{"developer", "user", "assistant", "tool:call_00_a", "tool:call_01_b", "notice", "notice", "user"}, ",")
	if got := strings.Join(seq, ","); got != want {
		t.Fatalf("消息序列变了\ngot : %s\nwant: %s", got, want)
	}

	// 图片不能因为重排而丢失
	for _, id := range []string{"call_00_a", "call_01_b"} {
		var parts []any
		for _, m := range msgs {
			if mm, _ := m.(map[string]any); mm != nil && toolResultCallID(mm) == id {
				parts, _ = mm["content"].([]any)
			}
		}
		if len(parts) != 1 {
			t.Fatalf("%s 的图片结果丢了：content=%#v", id, parts)
		}
	}
}

func TestAlreadyAdjacentBlockIsUntouched(t *testing.T) {
	raw := map[string]any{"input": []any{
		textItem("user", "北京和上海的天气"),
		callItem("call_00_a"),
		callItem("call_01_b"),
		outputItem("call_00_a", []any{map[string]any{"type": "input_text", "text": "晴"}}),
		outputItem("call_01_b", []any{map[string]any{"type": "input_text", "text": "雨"}}),
		textItem("user", "继续"),
	}}
	before := responsesInputToMessages(map[string]any{"input": raw["input"]})
	after := normalizeToolResultAdjacency(before)
	if len(after) != len(before) {
		t.Fatalf("无需重排却改了条数：%d → %d", len(before), len(after))
	}
	for i := range after {
		a, _ := after[i].(map[string]any)
		b, _ := before[i].(map[string]any)
		if a["role"] != b["role"] || a["tool_call_id"] != b["tool_call_id"] {
			t.Fatalf("第 %d 条被挪动：%v → %v", i, b["role"], a["role"])
		}
	}
}

func TestAdjacencyStopsAtForeignToolResult(t *testing.T) {
	// 上一组调用缺结果、紧接着是另一组的结果时，不能跨组把别人的结果搬过来。
	msgs := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_x", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
		map[string]any{"role": "system", "content": notice},
		map[string]any{"role": "tool", "tool_call_id": "call_y", "content": "别的组的结果"},
	}
	out := normalizeToolResultAdjacency(msgs)
	if len(out) != 3 {
		t.Fatalf("条数变了：%d", len(out))
	}
	last, _ := out[len(out)-1].(map[string]any)
	if toolResultCallID(last) != "call_y" {
		t.Fatalf("外来结果被搬动：%#v", last)
	}
}

func TestIsQuotaSignal(t *testing.T) {
	cases := []struct {
		code int
		msg  string
		want bool
	}{
		{6004, "usage exceeds frequency limit, but don't worry, your usage will reset at 2026-10-13 11:01:59 UTC+8", true},
		{14003, "too many requests", true},
		{11140, "request illegal", true},
		{0, `{"error":{"message":"too many requests"}}`, true},
		{11148, "tool calls and tool results do not match, please start a new conversation and retry", false},
		{11133, "the request parameters were rejected by the model provider", false},
		{11115, "prompt is too long: 1048742 tokens > 1048576 maximum", false},
		{11128, "Illegal API invocation from an unapproved channel", false},
		{11128, "first message is not system prompt", false},
		{11150, "the reasoning effort value is not supported by the current model", false},
	}
	for _, c := range cases {
		if got := isQuotaSignal(c.code, c.msg); got != c.want {
			t.Errorf("isQuotaSignal(%d, %q) = %v，期望 %v", c.code, truncate(c.msg, 40), got, c.want)
		}
	}
}

func rolesOf(msgs []any) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			parts = append(parts, "?")
			continue
		}
		if r, _ := mm["role"].(string); r != "" {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, ",")
}
