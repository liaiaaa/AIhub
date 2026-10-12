package main

// 客户端未指定思考档位时的默认档注入（2026-10-08 用户要求：默认最高）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeBody(s string) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	_ = dec.Decode(&m)
	return m
}

func TestApplyDefaultThinkingInjectsWhenSilent(t *testing.T) {
	raw := decodeBody(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	body := map[string]any{"model": "glm-5.3", "messages": raw["messages"], "stream": true}
	applyDefaultThinking(body, raw, raw["messages"].([]any), wbThinkingDefaultEffort)
	if body["reasoning_effort"] != wbThinkingDefaultEffort {
		t.Fatalf("未指定档位应补最高档，实际 body=%v", body)
	}
}

func TestApplyDefaultThinkingRespectsClient(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"显式 low", `{"reasoning_effort":"low"}`},
		{"显式 none", `{"reasoning_effort":"none"}`},
		{"thinking 对象", `{"thinking":{"type":"disabled"}}`},
		{"enable_thinking", `{"enable_thinking":false}`},
		{"budget", `{"reasoning_budget_tokens":1024}`},
		{"空串视同未指定但 effort 键存在", `{"effort":""}`},
	}
	for _, c := range cases {
		raw := decodeBody(`{"model":"m","messages":[{"role":"user","content":"hi"}],` + c.json[1:])
		body := map[string]any{"model": "m"}
		for _, k := range passthroughKeys {
			if v, ok := raw[k]; ok {
				body[k] = v
			}
		}
		applyDefaultThinking(body, raw, raw["messages"].([]any), wbThinkingDefaultEffort)
		got, _ := body["reasoning_effort"].(string)
		if c.name == "空串视同未指定但 effort 键存在" {
			// effort:"" 是空值 → 视同没发，应补默认档
			if got != wbThinkingDefaultEffort {
				t.Fatalf("%s: 期望补默认档，实际 %q", c.name, got)
			}
			continue
		}
		if got == wbThinkingDefaultEffort {
			t.Fatalf("%s: 客户端已表达意图，不该被改写为 %q", c.name, got)
		}
		if c.name == "显式 low" && got != "low" {
			t.Fatalf("显式 low 被改写：%q", got)
		}
	}
}

func TestApplyDefaultThinkingNoThinkMarker(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": qoderNoThinkMarker + " 你好"}}
	raw := map[string]any{"model": "m", "messages": msgs}
	body := map[string]any{"model": "m"}
	applyDefaultThinking(body, raw, msgs, wbThinkingDefaultEffort)
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("[no-thinking] 是主动关思考，不该注入档位：%v", body)
	}
}

func TestApplyDefaultThinkingDisabledForUnverifiedLines(t *testing.T) {
	raw := map[string]any{"model": "m", "messages": []any{}}
	body := map[string]any{}
	applyDefaultThinking(body, raw, []any{}, "")
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("ThinkingDefault 为空必须完全透传：%v", body)
	}
}
