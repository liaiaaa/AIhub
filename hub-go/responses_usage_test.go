package main

// 2026-10-21 开源审查修的 P1：Responses 的 usage 字段缺值时必须补 0，
// 不能输出 null —— Codex 的 usage 结构是 int64，null 反序列化会报错，
// 整条响应被判失败（即使正文是对的）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustNotContainNull(t *testing.T, m map[string]any) {
	t.Helper()
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), "null") {
		t.Errorf("usage 里出现 null（Codex 反序列化会失败）：%s", b)
	}
}

func TestResponsesUsageNil(t *testing.T) {
	u := responsesUsage(nil)
	mustNotContainNull(t, u)
	// 数值类型可能是 int / int64 / json.Number，比较前统一成 int64
	for _, k := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if got := asInt64(u[k]); got != 0 {
			t.Errorf("%s 全缺失时应为 0，得到 %v", k, u[k])
		}
	}
}

func TestResponsesUsagePartial(t *testing.T) {
	// 只有 prompt_tokens，缺 completion / total
	u := responsesUsage(map[string]any{"prompt_tokens": json.Number("100")})
	mustNotContainNull(t, u)
	if got := asInt64(u["output_tokens"]); got != 0 {
		t.Errorf("缺 completion_tokens 应补 0，得到 %v", u["output_tokens"])
	}
	// total 缺失时用 in+out 兜底
	if got := asInt64(u["total_tokens"]); got != 100 {
		t.Errorf("total 缺失应用 in+out 兜底为 100，得到 %v", got)
	}
}

func TestResponsesUsageDetailNil(t *testing.T) {
	// prompt_tokens_details 存在但 cached_tokens 缺失
	u := responsesUsage(map[string]any{
		"prompt_tokens":         json.Number("10"),
		"completion_tokens":     json.Number("5"),
		"total_tokens":          json.Number("15"),
		"prompt_tokens_details": map[string]any{},
	})
	mustNotContainNull(t, u)
	det, ok := u["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("input_tokens_details 形态不对：%v", u)
	}
	if got := asInt64(det["cached_tokens"]); got != 0 {
		t.Errorf("cached_tokens 缺失应补 0，得到 %v", det["cached_tokens"])
	}
}

func TestResponsesUsageFull(t *testing.T) {
	u := responsesUsage(map[string]any{
		"prompt_tokens":         json.Number("100"),
		"completion_tokens":     json.Number("20"),
		"total_tokens":          json.Number("120"),
		"prompt_tokens_details": map[string]any{"cached_tokens": json.Number("80")},
	})
	b, _ := json.Marshal(u)
	for _, want := range []string{`"input_tokens":100`, `"output_tokens":20`, `"total_tokens":120`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("完整 usage 被改坏，缺 %s：%s", want, b)
		}
	}
}

// asInt64 把 usage 数值统一成 int64 便于断言（生产代码里可能是
// json.Number / float64 / int / int64）。
func asInt64(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return -1
}
