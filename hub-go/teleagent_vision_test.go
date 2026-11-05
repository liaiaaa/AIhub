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

// 2026-11-04：用户反馈「旗舰（chat-flagship）看不见图，只能用 OCR」。
// 旗舰是纯文本模型，转文字是对的；但 chat-pro/chat-flash/chat-lite 能吃图，
// 不该也被转成文字 —— 所以默认档位是 auto（透传），always 才是「一律转文字」。
func TestTeleagentVisionMode(t *testing.T) {
	for _, v := range []string{"", "auto", "on", "true"} {
		t.Setenv("AIHUB_TELEAGENT_VISION", v)
		if got := teleagentVisionMode(); got != "auto" {
			t.Errorf("%q 应为 auto，实际 %s", v, got)
		}
	}
	for _, v := range []string{"always", "force", "1"} {
		t.Setenv("AIHUB_TELEAGENT_VISION", v)
		if got := teleagentVisionMode(); got != "always" {
			t.Errorf("%q 应为 always，实际 %s", v, got)
		}
	}
}

// 能吃图的模型走透传，纯文本模型才转文字 —— 这条断言直接对着真实模型表。
func TestVisionAutoKeepsNativeImageModels(t *testing.T) {
	if !modelSupportsImage("teleagent", "chat-pro") {
		t.Error("chat-pro 应支持图片（透传）")
	}
	if !modelSupportsImage("teleagent", "chat-lite") {
		t.Error("chat-lite 应支持图片（它是桌面端识图模型）")
	}
	if modelSupportsImage("teleagent", "chat-flagship") {
		t.Error("chat-flagship 是纯文本模型，应转文字")
	}
}

// TestVisionHeadersCarryTimestampAndNonce 钉住 2026-11-05 修的那个签名 bug。
//
// 之前 askTeleagentVisionDirect 调 teleagentSignHeadersAt(..., ts="", nonce="")，
// 结果 X-SuperAgent-Timestamp / X-SuperAgent-Nonce 是**空串**发出去，
// 上游按时间戳+nonce 校验直接判废 → 日志 242 次「识图没有返回内容」，
// 每张带图请求白烧两次上游往返。
func TestVisionHeadersCarryTimestampAndNonce(t *testing.T) {
	tok := "aaa.bbb.ccc" // 三段即算合法（签名只用第三段做密钥）
	hdrs, err := teleagentVisionHeaders(tok, "dev-1", "inst-1", "/superCowork/sapi/api/v1/chat/completions", []byte(`{"model":"chat-lite"}`))
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}
	ts := hdrs["X-SuperAgent-Timestamp"]
	nonce := hdrs["X-SuperAgent-Nonce"]
	if ts == "" {
		t.Fatal("X-SuperAgent-Timestamp 不能为空（这就是之前的 bug）")
	}
	if nonce == "" {
		t.Fatal("X-SuperAgent-Nonce 不能为空")
	}
	// ts 必须是秒级时间戳，不是毫秒/日期
	if len(ts) != 10 {
		t.Errorf("时间戳应是 10 位秒级，得到 %q", ts)
	}
	// nonce 应是 UUIDv4 形状
	if len(nonce) != 36 || nonce[14] != '4' {
		t.Errorf("nonce 应是 UUIDv4，得到 %q", nonce)
	}
	if hdrs["X-SuperAgent-Signature"] == "" {
		t.Error("签名为空")
	}
	// 两次调用的 nonce 必须不同（防重放字段不能恒定）
	h2, _ := teleagentVisionHeaders(tok, "dev-1", "inst-1", "/x", []byte(`{}`))
	if h2["X-SuperAgent-Nonce"] == nonce {
		t.Error("nonce 每次都一样，等于没防重放")
	}
}
