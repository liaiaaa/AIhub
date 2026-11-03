package main

// 2026-11-03：DSH 走 TeleAgent 时的两处 400，都是消息形态问题（不是图片格式）。

import (
	"strings"
	"testing"
)

func TestTeleagentNormalizeDeveloperRole(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "developer", "content": "你是助手"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	teleagentNormalizeMessages(payload)
	msgs := payload["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("developer 应降级成 system，实际 %v", first["role"])
	}
}

func TestTeleagentNormalizeDropsUnusableImages(t *testing.T) {
	ok := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}
	httpImg := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}}
	bad := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "attachment://abc"}}
	empty := map[string]any{"type": "image_url", "image_url": map[string]any{"url": ""}}
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "看图"},
			ok, httpImg, bad, empty,
		}},
		map[string]any{"role": "user", "content": []any{bad}}, // 全被丢掉 → 要有占位
	}}
	teleagentNormalizeMessages(payload)
	msgs := payload["messages"].([]any)
	parts := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Errorf("应保留 文本 + data 图 + http 图 共 3 个 part，实际 %d", len(parts))
	}
	for _, raw := range parts {
		pm := raw.(map[string]any)
		if pm["type"] == "image_url" {
			u := pm["image_url"].(map[string]any)["url"].(string)
			if strings.HasPrefix(u, "attachment://") || u == "" {
				t.Errorf("不可用的图片引用应被丢掉：%q", u)
			}
		}
	}
	second := msgs[1].(map[string]any)["content"].([]any)
	if len(second) != 1 {
		t.Fatalf("全丢时应有占位 part，实际 %d", len(second))
	}
	p := second[0].(map[string]any)
	if p["type"] != "text" || !strings.Contains(p["text"].(string), "图片") {
		t.Errorf("占位 part 不对：%v", p)
	}
}

func TestTeleagentNormalizeKeepsPlainContent(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "普通字符串不该被动"},
	}}
	teleagentNormalizeMessages(payload)
	m := payload["messages"].([]any)[0].(map[string]any)
	if m["content"] != "普通字符串不该被动" {
		t.Errorf("字符串 content 被改了：%v", m["content"])
	}
}

// DSH 实际发来的图片 part 有两种写法，字符串形态上游会 400，必须归一化成对象。
func TestTeleagentNormalizeImageURLStringForm(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "看图"},
			map[string]any{"type": "image_url", "image_url": "data:image/webp;base64,AAAA"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,BBBB"},
		}},
	}}
	teleagentNormalizeMessages(payload)
	parts := payload["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("应保留 3 个 part，实际 %d", len(parts))
	}
	for _, raw := range parts[1:] {
		pm := raw.(map[string]any)
		if pm["type"] != "image_url" {
			t.Errorf("input_image 应被改成 image_url，实际 %v", pm["type"])
		}
		iu, ok := pm["image_url"].(map[string]any)
		if !ok {
			t.Fatalf("image_url 应是对象形态，实际 %T", pm["image_url"])
		}
		if u, _ := iu["url"].(string); !strings.HasPrefix(u, "data:") {
			t.Errorf("url 应保留 data URL，实际 %q", u)
		}
	}
}

// 上游坚持拒收图片时，去掉图片重试不应该再带 image part。
func TestStripImageParts(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "看图"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}},
		}},
	}}
	if !hasImageParts(payload) {
		t.Fatal("应识别出有图片")
	}
	b, ok := stripImageParts(payload)
	if !ok {
		t.Fatal("应产生去掉图片后的请求体")
	}
	if strings.Contains(string(b), "image_url") {
		t.Errorf("去图后不应再有 image_url：%s", string(b)[:min(120, len(b))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
