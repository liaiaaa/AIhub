package main

// 2026-11-03：TeleAgent 的图片走「视觉模型转文字」，不是把图塞给对话模型
// （桌面端 runtime 里 /image-read 就是这么做的）。这里只测不依赖网络的骨架部分。

import (
	"strings"
	"testing"
)

func TestTeleagentVisionEnabledDefault(t *testing.T) {
	t.Setenv("AIHUB_TELEAGENT_VISION", "")
	if !teleagentVisionEnabled() {
		t.Error("默认应开启")
	}
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("AIHUB_TELEAGENT_VISION", v)
		if teleagentVisionEnabled() {
			t.Errorf("%q 应关闭", v)
		}
	}
}

func TestVisionModelCandidatesFromEnv(t *testing.T) {
	t.Setenv("AIHUB_VISION_MODEL", "my.vision-model")
	got := visionModelCandidates()
	if len(got) != 1 || got[0] != "my.vision-model" {
		t.Errorf("应只用一个指定模型，实际 %v", got)
	}
	t.Setenv("AIHUB_VISION_MODEL", "")
	if len(visionModelCandidates()) < 2 {
		t.Error("未指定时应有多候选兜底")
	}
}

// 非 data URL 的图片引用没法定制，必须被丢掉；全丢时要补占位文本。
func TestReplaceImagesDropsNonDataURL(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "看图"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "attachment://x"}},
		}},
	}}
	n, ok := replaceImagesWithDescriptions(payload, func(string, ...any) {})
	if ok || n != 0 {
		t.Errorf("没有可转换的图片时应返回 ok=false，实际 n=%d ok=%v", n, ok)
	}
	parts := payload["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("应只剩文本 part，实际 %d", len(parts))
	}
	if !strings.Contains(parts[0].(map[string]any)["text"].(string), "看图") {
		t.Error("文本 part 不该被改")
	}
}

func TestDataURLBytes(t *testing.T) {
	if b := dataURLBytes("data:image/png;base64,AAAA"); len(b) != 3 {
		t.Errorf("应解出 3 字节，实际 %d", len(b))
	}
	if b := dataURLBytes("attachment://x"); b != nil {
		t.Error("非 data URL 应返回 nil")
	}
}
