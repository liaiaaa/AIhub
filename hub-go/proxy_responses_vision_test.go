package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 图片翻译回归（2026-10-12 用户报的 bug）。
//
// 背景：Codex 走聚合口 :8790（wire_api=responses）时用
// {"type":"input_image","image_url":"data:image/png;base64,..."} 传图，
// 而 responsesContentText 只认文本片段 → 图片被静默跳过；纯图片消息还会因
// 「文本为空」被整条丢弃。实测同一张图直发 chat 格式上游能正确识别
// （答出图中央颜色），经本层翻译后却答「没有收到图片」。
const visionDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

func TestResponsesContentChatKeepsImage(t *testing.T) {
	// 纯文本：必须仍返回 string（绝大多数请求走这条，形态不能变）
	got := responsesContentChat([]any{
		map[string]any{"type": "input_text", "text": "你好"},
	})
	if s, ok := got.(string); !ok || s != "你好" {
		t.Fatalf("纯文本应返回 string，实际 %#v", got)
	}

	// 文本 + 图片：返回 parts 数组，两者都在，且顺序为 text → image
	got = responsesContentChat([]any{
		map[string]any{"type": "input_text", "text": "这个报错啥意思"},
		map[string]any{"type": "input_image", "image_url": visionDataURL},
	})
	parts, ok := got.([]any)
	if !ok {
		t.Fatalf("含图应返回数组，实际 %#v", got)
	}
	if len(parts) != 2 {
		t.Fatalf("应有 2 个 part，实际 %d：%#v", len(parts), parts)
	}
	p0 := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "这个报错啥意思" {
		t.Fatalf("part0 = %#v", p0)
	}
	p1 := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Fatalf("part1 类型 = %v", p1["type"])
	}
	iu := p1["image_url"].(map[string]any)
	if iu["url"] != visionDataURL {
		t.Fatalf("图片 URL 丢失: %#v", iu)
	}
	if iu["detail"] != "high" {
		t.Fatalf("detail 默认档应为 high，实际 %v", iu["detail"])
	}
}

// 纯图片消息（没有任何文本）必须留下——原 bug 就是在这里被整条丢掉的。
func TestResponsesInputKeepsImageOnlyMessage(t *testing.T) {
	raw := map[string]any{
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{
					map[string]any{"type": "input_image", "image_url": visionDataURL},
				},
			},
		},
	}
	msgs := responsesInputToMessages(raw)
	if len(msgs) != 1 {
		t.Fatalf("纯图片消息被丢弃：msgs = %d 条", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" {
		t.Fatalf("role = %v", m["role"])
	}
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content 应保留 1 个图片 part，实际 %#v", m["content"])
	}
	if parts[0].(map[string]any)["type"] != "image_url" {
		t.Fatalf("part = %#v", parts[0])
	}
}

// 空消息（既无文本也无图）仍应被剔除，别把噪音塞给上游。
func TestResponsesInputDropsTrulyEmptyMessage(t *testing.T) {
	raw := map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{}},
			map[string]any{"type": "message", "role": "user", "content": "  "},
			map[string]any{"type": "message", "role": "user", "content": "有效"},
		},
	}
	msgs := responsesInputToMessages(raw)
	if len(msgs) != 1 {
		t.Fatalf("应只留 1 条有效消息，实际 %d 条", len(msgs))
	}
	if msgs[0].(map[string]any)["content"] != "有效" {
		t.Fatalf("留下的不是那条有效消息：%#v", msgs[0])
	}
}

// developer/system 的降敏要能穿过 parts 数组，且不能把图片弄丢。
func TestSanitizeHarnessPromptKeepsImage(t *testing.T) {
	in := []any{
		map[string]any{"type": "text", "text": "You are OpenAI harness"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": visionDataURL}},
	}
	out := sanitizeHarnessPromptAny(in)
	parts, ok := out.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("降敏后 part 数变了：%#v", out)
	}
	txt := parts[0].(map[string]any)["text"].(string)
	if strings.Contains(txt, "OpenAI") || !strings.Contains(txt, "Open AI") {
		t.Fatalf("文本未降敏：%q", txt)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("图片 part 被降敏流程弄丢：%#v", parts[1])
	}
}

// 上游不认 "original"，不能原样透传。
func TestChatImageDetailFallsBackFromOriginal(t *testing.T) {
	got := responsesContentChat([]any{
		map[string]any{"type": "input_image", "image_url": visionDataURL, "detail": "original"},
	})
	parts := got.([]any)
	iu := parts[0].(map[string]any)["image_url"].(map[string]any)
	if iu["detail"] != "high" {
		t.Fatalf("original 应退到 high，实际 %v", iu["detail"])
	}

	// 显式 low 要尊重，别乱改
	got = responsesContentChat([]any{
		map[string]any{"type": "input_image", "image_url": visionDataURL, "detail": "low"},
	})
	parts = got.([]any)
	iu = parts[0].(map[string]any)["image_url"].(map[string]any)
	if iu["detail"] != "low" {
		t.Fatalf("显式 low 被改写为 %v", iu["detail"])
	}
}

// chat 形态的 {"image_url":{"url":...}} 也要认（聚合口内转发时会出现）。
func TestChatImageURLNestedForm(t *testing.T) {
	got := responsesContentChat([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": visionDataURL, "detail": "low"}},
	})
	parts := got.([]any)
	iu := parts[0].(map[string]any)["image_url"].(map[string]any)
	if iu["url"] != visionDataURL || iu["detail"] != "low" {
		t.Fatalf("嵌套形态解析失败：%#v", iu)
	}
}

// 图片夹在文本中间时，顺序必须保持 text → image → text。
func TestResponsesContentChatPreservesOrder(t *testing.T) {
	got := responsesContentChat([]any{
		map[string]any{"type": "input_text", "text": "前"},
		map[string]any{"type": "input_image", "image_url": visionDataURL},
		map[string]any{"type": "input_text", "text": "后"},
	})
	parts := got.([]any)
	if len(parts) != 3 {
		t.Fatalf("应有 3 个 part，实际 %d：%#v", len(parts), parts)
	}
	types := make([]string, len(parts))
	for i, p := range parts {
		types[i] = p.(map[string]any)["type"].(string)
	}
	want := []string{"text", "image_url", "text"}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("顺序错误：%v，应为 %v", types, want)
		}
	}
}

// 走一遍 responsesBodyToChatBody（聚合口 qoder/trae 分支用的就是它），
// 确认图片能穿过整条翻译链到达最终 chat body。
func TestResponsesBodyToChatBodyKeepsImage(t *testing.T) {
	body := map[string]any{
		"model": "qoder.Qwen3.8-Flash",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "看图"},
					map[string]any{"type": "input_image", "image_url": visionDataURL},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	cb, err := responsesBodyToChatBody(raw)
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	msgs := cb["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("消息数 = %d", len(msgs))
	}
	parts, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("content 不是数组，图片丢了：%#v", msgs[0])
	}
	found := false
	for _, p := range parts {
		if p.(map[string]any)["type"] == "image_url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("最终 chat body 里没有 image_url：%#v", parts)
	}
}
