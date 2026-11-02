package main

// 2026-11-02：入站图片降采样与字节闸的回归锁定。
//
// 起因：Codex 会话里 51 张 base64 截图（31MB）每轮全量重发，请求体 30.8MB
// 被 qoder 上游 413 拒收；且 aihub 还会对 413 做一次「非流式回退」重试，
// 等于把同样的 30.8MB 再传一遍（白烧一倍流量）。这两个测试钉住兜底逻辑。

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// makeTestPNG 生成一张 w×h 的 PNG（带渐变，避免纯色被压得太小导致测不出效果）。
func makeTestPNG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x * 255) / w),
				G: uint8((y * 255) / h),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成测试 PNG 失败：%v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func decodeDataURLImage(t *testing.T, s string) image.Image {
	t.Helper()
	_, b64, ok := splitDataURL(s)
	if !ok {
		t.Fatalf("不是合法 data URL：%.40s", s)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 解码失败：%v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("图片解码失败：%v", err)
	}
	return img
}

// TestShrinkDataURLImagesDownscales 大图必须被缩到长边上限以内，体积明显下降。
func TestShrinkDataURLImagesDownscales(t *testing.T) {
	t.Setenv("AIHUB_IMG_SHRINK", "")
	t.Setenv("AIHUB_IMG_SHRINK_MIN_MB", "0")
	t.Setenv("AIHUB_IMG_MAX_SIDE", "")
	t.Setenv("AIHUB_IMG_MIN_KB", "4") // 测试图不大，把门槛降下来

	big := makeTestPNG(t, 2400, 1600)
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "看看这张图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": big}},
			}},
		},
	}
	n, before, after := shrinkDataURLImagesIn(body)
	if n != 1 {
		t.Fatalf("应处理 1 张图，实际 %d", n)
	}
	if after >= before {
		t.Fatalf("体积没降：%d → %d", before, after)
	}
	// 取出处理后的图，验证尺寸与可解码性
	msg := body["messages"].([]any)[0].(map[string]any)
	parts := msg["content"].([]any)
	url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	img := decodeDataURLImage(t, url)
	b := img.Bounds()
	if b.Dx() > 1024 || b.Dy() > 1024 {
		t.Errorf("长边应 <= 1024，实际 %dx%d", b.Dx(), b.Dy())
	}
	if b.Dx() != 1024 && b.Dy() != 1024 {
		t.Errorf("应等比缩放到长边 1024，实际 %dx%d", b.Dx(), b.Dy())
	}
	// 文本段必须原样保留
	if parts[0].(map[string]any)["text"] != "看看这张图" {
		t.Error("文本段被改动了")
	}
}

// TestShrinkSkipsSmallImages 小图不动（避免无意义的重编码）。
func TestShrinkSkipsSmallImages(t *testing.T) {
	t.Setenv("AIHUB_IMG_SHRINK", "")
	t.Setenv("AIHUB_IMG_SHRINK_MIN_MB", "0")
	t.Setenv("AIHUB_IMG_MIN_KB", "96")
	small := makeTestPNG(t, 40, 40)
	body := map[string]any{"url": small}
	if n, _, _ := shrinkDataURLImagesIn(body); n != 0 {
		t.Fatalf("小图不该被处理，实际处理了 %d 张", n)
	}
	if body["url"].(string) != small {
		t.Error("小图内容被改动了")
	}
}

// TestImageShrinkEnvKnobs 环境变量能改长边上限，也能整体关掉。
func TestImageShrinkEnvKnobs(t *testing.T) {
	t.Setenv("AIHUB_IMG_MIN_KB", "4")
	t.Setenv("AIHUB_IMG_MAX_SIDE", "512")
	t.Setenv("AIHUB_IMG_SHRINK_MIN_MB", "0")
	big := makeTestPNG(t, 1600, 1200)
	body := map[string]any{"url": big}
	if n, _, _ := shrinkDataURLImagesIn(body); n != 1 {
		t.Fatalf("应处理 1 张，实际 %d", n)
	}
	img := decodeDataURLImage(t, body["url"].(string))
	if b := img.Bounds(); b.Dx() > 512 || b.Dy() > 512 {
		t.Errorf("AIHUB_IMG_MAX_SIDE=512 未生效：%dx%d", b.Dx(), b.Dy())
	}

	// 关掉：任何图都不动
	t.Setenv("AIHUB_IMG_SHRINK", "0")
	body2 := map[string]any{"url": makeTestPNG(t, 1600, 1200)}
	if n, _, _ := shrinkDataURLImagesIn(body2); n != 0 {
		t.Fatalf("AIHUB_IMG_SHRINK=0 时不该处理，实际 %d", n)
	}
}

// TestTrimOldDataURLImages 超限时按「最老优先」裁剪，最近的图保住。
func TestTrimOldDataURLImages(t *testing.T) {
	t.Setenv("AIHUB_IMG_SHRINK", "")
	t.Setenv("AIHUB_IMG_SHRINK_MIN_MB", "0")
	// 三张图，每张约 30KB 级；限制压到只够留一张多一点
	imgs := []string{makeTestPNG(t, 400, 300), makeTestPNG(t, 401, 300), makeTestPNG(t, 402, 300)}
	total := 0
	for _, s := range imgs {
		total += len(s)
	}
	limit := total/2 - 100
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgs[0]}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgs[1]}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgs[2]}},
			}},
		},
	}
	if n := trimOldDataURLImagesIn(body, limit); n < 1 {
		t.Fatalf("应至少裁掉 1 张，实际 %d", n)
	}
	parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	first := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	last := parts[2].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if first != imagePlaceholderURL {
		t.Error("最老的那张应被换成占位图")
	}
	if last != imgs[2] {
		t.Error("最新的那张不该被裁")
	}
	// 剩下的总字节必须落到上限以内
	rest := 0
	for _, p := range parts {
		rest += len(p.(map[string]any)["image_url"].(map[string]any)["url"].(string))
	}
	if rest > limit {
		t.Errorf("裁剪后仍超限：%d > %d", rest, limit)
	}
}

// TestPlaceholderIsValidImage 占位图必须是合法可解码的图（否则上游会挑刺）。
func TestPlaceholderIsValidImage(t *testing.T) {
	img := decodeDataURLImage(t, imagePlaceholderURL)
	if b := img.Bounds(); b.Dx() != 1 || b.Dy() != 1 {
		t.Errorf("占位图应为 1x1，实际 %dx%d", b.Dx(), b.Dy())
	}
	if !strings.HasPrefix(imagePlaceholderURL, "data:image/png;base64,") {
		t.Errorf("占位图格式不对：%.40s", imagePlaceholderURL)
	}
}

// TestQoderDeterministic4xx 413/400/422/404 不重试；401/403/429/5xx 不在其中。
func TestQoderDeterministic4xx(t *testing.T) {
	for _, code := range []int{413, 400, 422, 404} {
		if !qoderDeterministic4xx(code) {
			t.Errorf("%d 应判为确定性失败", code)
		}
	}
	for _, code := range []int{401, 403, 408, 429, 500, 502, 503} {
		if qoderDeterministic4xx(code) {
			t.Errorf("%d 不该判为确定性失败", code)
		}
	}
	if got := fmt.Sprint(qoderDeterministic4xx(413)); got != "true" {
		t.Fatalf("413 判定异常：%s", got)
	}
}
