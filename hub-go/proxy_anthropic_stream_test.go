package main

// Anthropic 流式块索引的回归测试（2026-11-09）。
//
// 用户实测：Claude Code CLI 经 aihub 打 qoder 线路，工具调用报
// 「The model's tool call could not be parsed (retry also failed)」，
// 而同一请求直接打线路口是正常的。
//
// 根因：anthStreamState.blockIdx 从 0 起，而 openText/openThinking/工具块
// 都是「先 ++ 再 emit」，于是**首个内容块 index=1**。Anthropic 协议要求
// 首个块 index=0，Claude Code 按 index 0 找块，找不到就当解析失败。
// 上面 20:13 的抓包就是这个形状：
//
//	{"content_block":{"type":"tool_use",...},"index":1}   ← 应为 0

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// flushRecorder 把 http.ResponseWriter 的最小实现接到 strings.Builder 上：
// anthropicStreamResponse 只看 Write + 可选的 Flusher，不需要完整 ResponseWriter。
type flushRecorder struct{ b *strings.Builder }

func (f *flushRecorder) Header() http.Header         { return http.Header{} }
func (f *flushRecorder) Write(p []byte) (int, error) { return f.b.Write(p) }
func (f *flushRecorder) WriteHeader(int)             {}
func (f *flushRecorder) Flush()                      {}

// sseEvents 把 SSE 文本拆成 (event, data) 序列。
func sseEvents(t *testing.T, raw string) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]any
	}
	var ev string
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			ev = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("坏帧：%v（%s）", err, line)
			}
			out = append(out, struct {
				Event string
				Data  map[string]any
			}{ev, m})
		}
	}
	return out
}

// chatSSE 拼一条 chat/completions 的流式响应。
func chatSSE(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: ")
		b.WriteString(c)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func collectIndices(t *testing.T, raw string) (starts []int, deltas []int) {
	t.Helper()
	for _, e := range sseEvents(t, raw) {
		idx, ok := e.Data["index"]
		if !ok {
			continue
		}
		f, _ := idx.(float64)
		switch e.Event {
		case "content_block_start":
			starts = append(starts, int(f))
		case "content_block_delta":
			deltas = append(deltas, int(f))
		}
	}
	return
}

func TestAnthropicStreamToolBlockStartsAtZero(t *testing.T) {
	body := chatSSE(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"echo hi\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	var sb strings.Builder
	anthropicStreamResponse(&flushRecorder{&sb}, strings.NewReader(body), "claude-sonnet-4-6", false)
	starts, deltas := collectIndices(t, sb.String())

	if len(starts) != 1 || starts[0] != 0 {
		t.Fatalf("首个 content_block_start 必须是 index=0，得到 %v", starts)
	}
	for _, d := range deltas {
		if d != 0 {
			t.Fatalf("工具块的 delta 应指向 index=0，得到 %v", deltas)
		}
	}
}

func TestAnthropicStreamTextBlockStartsAtZero(t *testing.T) {
	body := chatSSE(
		`{"choices":[{"delta":{"content":"你"}}]}`,
		`{"choices":[{"delta":{"content":"好"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	var sb strings.Builder
	anthropicStreamResponse(&flushRecorder{&sb}, strings.NewReader(body), "claude-sonnet-4-6", false)
	starts, _ := collectIndices(t, sb.String())
	if len(starts) != 1 || starts[0] != 0 {
		t.Fatalf("文本块应从 index=0 开始，得到 %v", starts)
	}
}

func TestAnthropicStreamThinkingThenTextIndexesAreSequential(t *testing.T) {
	body := chatSSE(
		`{"choices":[{"delta":{"reasoning_content":"想一下"}}]}`,
		`{"choices":[{"delta":{"content":"答案"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	var sb strings.Builder
	anthropicStreamResponse(&flushRecorder{&sb}, strings.NewReader(body), "claude-sonnet-4-6", true)
	starts, _ := collectIndices(t, sb.String())
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 1 {
		t.Fatalf("thinking→text 应为 index 0,1，得到 %v", starts)
	}
}

func TestAnthropicStreamTextThenToolIndexes(t *testing.T) {
	// Code CLI 常见形态：先说一句，再调工具。块索引必须连续且从 0 起。
	body := chatSSE(
		`{"choices":[{"delta":{"content":"我来执行"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"Bash","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	var sb strings.Builder
	anthropicStreamResponse(&flushRecorder{&sb}, strings.NewReader(body), "claude-sonnet-4-6", false)
	starts, _ := collectIndices(t, sb.String())
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 1 {
		t.Fatalf("text→tool 应为 index 0,1，得到 %v", starts)
	}
}
