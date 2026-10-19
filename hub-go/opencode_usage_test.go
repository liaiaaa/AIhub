package main

// OpenCode 免费档的用量记账。
//
// 此前用量统计只在聚合口记（recordTap 走 proxy_agg.go），而 oc 的模型没进聚合
// 白名单，所以控制台「用量统计」里 OpenCode 一行都没有 —— 用户 2026-10-16 问起，
// 拿真实台账验证过：usage.jsonl 里 line 只有 intl/qoder/dom/trae/agg/official，
// oc 计数为 0。现在直连 :8800 的请求也自己落一行。

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 转发字节必须一字不改（客户端要看到完整 SSE），同时把 usage 捞出来。
func TestOpencodePipeStreamPassesThroughAndTapsUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":1036,"completion_tokens":32,"total_tokens":1068,"prompt_tokens_details":{"cached_tokens":1024},"completion_tokens_details":{"reasoning_tokens":28}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	usage := opencodePipeStream(rec, strings.NewReader(sse))

	if got := rec.Body.String(); got != sse {
		t.Errorf("转发内容被改了：\n got=%q\nwant=%q", got, sse)
	}
	if usage == nil {
		t.Fatal("没从流里捞出 usage —— 用量统计会是 0 token")
	}
	if usageInt(usage, "prompt_tokens", "input_tokens") != 1036 {
		t.Errorf("prompt_tokens 捞错：%v", usage)
	}
	if usageInt(usage, "completion_tokens", "output_tokens") != 32 {
		t.Errorf("completion_tokens 捞错：%v", usage)
	}
}

// responses 协议把 usage 嵌在 response 对象里，形状不同，也必须捞到。
func TestOpencodePipeStreamTapsNestedUsage(t *testing.T) {
	sse := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":700,\"output_tokens\":40}}}\n\n"
	rec := httptest.NewRecorder()
	usage := opencodePipeStream(rec, strings.NewReader(sse))
	if usage == nil {
		t.Fatal("responses 形态的嵌套 usage 没捞到")
	}
	if usageInt(usage, "prompt_tokens", "input_tokens") != 700 {
		t.Errorf("input_tokens 捞错：%v", usage)
	}
}

// 没有 usage 的流不该 panic，也不该写出假数字。
func TestOpencodePipeStreamWithoutUsage(t *testing.T) {
	rec := httptest.NewRecorder()
	usage := opencodePipeStream(rec, strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	if usage != nil {
		t.Errorf("不该凭空造出 usage：%v", usage)
	}
	recordOpencodeUsage("cat-jam-free", "chat", 0, 12, nil) // 必须能吃下 nil
}

func TestRecordOpencodeUsageLandsInLedger(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })

	usage := map[string]any{
		"prompt_tokens":            float64(1036),
		"completion_tokens":        float64(32),
		"total_tokens":             float64(1068),
		"prompt_tokens_details":    map[string]any{"cached_tokens": float64(1024)},
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(28)},
	}
	recordOpencodeUsage("cat-jam-free", "chat", 200, 2400, usage)
	recordOpencodeUsage("space-bunny-free", "responses", 0, 900, nil)

	b, err := os.ReadFile(filepath.Join(dir, usageFile))
	if err != nil {
		t.Fatalf("台账没写出来：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行，实际 %d：%s", len(lines), string(b))
	}
	var first usageRec
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	// Line 必须是 "oc"：用量统计按 line 分组并用 aggLineCN 翻译，写错就整条不显示。
	if first.Line != "oc" {
		t.Errorf("line = %q，应为 oc", first.Line)
	}
	if first.Slug != "oc.cat-jam-free" || first.Backend != "cat-jam-free" {
		t.Errorf("slug/backend 形态不对：%q / %q", first.Slug, first.Backend)
	}
	if first.In != 1036 || first.Out != 32 || first.Total != 1068 || first.Cached != 1024 || first.Reason != 28 {
		t.Errorf("token 拆解错：%+v", first)
	}
	if first.MS != 2400 {
		t.Errorf("耗时没记上：%d", first.MS)
	}
	var second usageRec
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second.Code != 200 {
		t.Errorf("code 传 0 应按 200 记，得到 %d", second.Code)
	}
}

// 台账里的 oc 必须能被报表分组认出来（aggLineCN 漏键的话前端只显示英文短码）。
func TestOcLineHasChineseLabel(t *testing.T) {
	if aggLineCN["oc"] == "" {
		t.Fatal("aggLineCN 没有 oc 的中文名，用量统计里会显示裸 \"oc\"")
	}
}
