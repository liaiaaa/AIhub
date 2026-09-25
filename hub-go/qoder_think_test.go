package main

// <think> 分流器测试：标签内容 → reasoning_content，标签外 → content。
// 重点覆盖跨帧截断（一个 chunk 只有半个标签）这类流式处理最容易错的场景。

import (
	"encoding/json"
	"strings"
	"testing"
)

func splitAll(chunks []string, final bool) (content, reasoning string) {
	st := newQoderThinkState()
	var sb, rb strings.Builder
	for _, c := range chunks {
		o, t := st.split(c)
		sb.WriteString(o)
		rb.WriteString(t)
	}
	if final {
		o, t := st.flush()
		sb.WriteString(o)
		rb.WriteString(t)
	}
	return sb.String(), rb.String()
}

func TestThinkSplitBasic(t *testing.T) {
	cases := []struct {
		name          string
		chunks        []string
		wantContent   string
		wantReasoning string
	}{
		{"无标签", []string{"你好", "！"}, "你好！", ""},
		{"完整标签单帧", []string{"<think>推理</think>答案"}, "答案", "推理"},
		{"标签前有正文", []string{"先说两句<think>想想</think>结论"}, "先说两句结论", "想想"},
		{"标签跨帧", []string{"<thi", "nk>深", "度</th", "ink>好"}, "好", "深度"},
		{"未闭合标签", []string{"<think>一路想到底"}, "", "一路想到底"},
		{"尾标签截断", []string{"<think>abc</th", "ink>ok"}, "ok", "abc"},
	}
	for _, c := range cases {
		gotC, gotR := splitAll(c.chunks, true)
		if gotC != c.wantContent || gotR != c.wantReasoning {
			t.Errorf("%s: content=%q reasoning=%q, want %q/%q",
				c.name, gotC, gotR, c.wantContent, c.wantReasoning)
		}
	}
}

// 逐帧改写：reasoning_content 正确追加，content 正确替换/删除。
func TestRewriteChunkStream(t *testing.T) {
	st := newQoderThinkState()
	mk := func(delta map[string]any) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
		return string(b)
	}
	frame1 := qoderRewriteChunk(mk(map[string]any{"content": "<think>第一段"}), st, false)
	var f1 struct {
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent string  `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(frame1), &f1); err != nil {
		t.Fatalf("解析改写帧失败: %v", err)
	}
	if f1.Choices[0].Delta.Content != nil && *f1.Choices[0].Delta.Content != "" {
		t.Errorf("纯思考帧 content 应为空/缺失，实际 %v", f1.Choices[0].Delta.Content)
	}
	if f1.Choices[0].Delta.ReasoningContent != "第一段" {
		t.Errorf("reasoning_content = %q, want 第一段", f1.Choices[0].Delta.ReasoningContent)
	}

	frame2 := qoderRewriteChunk(mk(map[string]any{"content": "Second</think>答案"}), st, false)
	var f2 struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(frame2), &f2); err != nil {
		t.Fatalf("解析改写帧失败: %v", err)
	}
	if f2.Choices[0].Delta.ReasoningContent != "Second" {
		t.Errorf("reasoning_content = %q, want Second", f2.Choices[0].Delta.ReasoningContent)
	}
	if f2.Choices[0].Delta.Content != "答案" {
		t.Errorf("content = %q, want 答案", f2.Choices[0].Delta.Content)
	}
}

// 非流式整段 SSE 转写：聚合后 message.reasoning_content 应包含完整思考。
func TestRewriteStreamForThinking(t *testing.T) {
	frame := func(content string) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": content},
		}}})
		return "data: " + string(b) + "\n\n"
	}
	sse := frame("<think>") + frame("分 step 推理。") + frame("</think>最终答案。") + "data: [DONE]\n\n"
	rewritten := qoderRewriteStreamForThinking(sse)
	col := collectStream(strings.NewReader(rewritten))
	res := col.build()
	msg := res["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got := msg["reasoning_content"].(string); got != "分 step 推理。" {
		t.Errorf("reasoning_content = %q, want 分 step 推理。", got)
	}
	if got := msg["content"].(string); got != "最终答案。" {
		t.Errorf("content = %q, want 最终答案。", got)
	}
}
