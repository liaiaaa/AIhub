package main

// 2026-10-19：上游 /v3/config 的 maxInputTokens 与 maxOutputTokens 是两个字段，
// 早期只解析了前者却塞进「输出上限」语义的字段，导致聚合网关把
// max_output_tokens 报成 1000000（客户端会以为模型能吐 100 万 token）。
//
// 真实数据（copilot.tencent.com，glm-5.3-flash）：
//   maxInputTokens = 1000000
//   maxOutputTokens = 131072
//   maxAllowedSize = 1000000

import (
	"testing"
)

func TestParseWBConfigSplitsInputAndOutputLimits(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[
		{"id":"glm-5.3-flash","name":"GLM-5.3-Flash","credits":"x0.06 credits",
		 "maxInputTokens":1000000,"maxOutputTokens":131072,"maxAllowedSize":1000000}
	]}}`)
	cat, err := parseWBConfigBody(body, "dom")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cat.byID()["glm-5.3-flash"]
	if !ok {
		t.Fatal("模型未解析")
	}
	// 输入上限
	if e.MaxInputTokens != 1000000 {
		t.Errorf("MaxInputTokens = %d，应为 1000000", e.MaxInputTokens)
	}
	// 输出上限必须是 131072，不能被输入上限污染
	if e.MaxOutput != 131072 {
		t.Errorf("MaxOutput = %d，应为 131072（不是输入上限 1000000）", e.MaxOutput)
	}
	// 历史字段 MaxInput 保持「输出上限」语义，供 model_meta / 聚合网关使用
	if e.MaxInput != 131072 {
		t.Errorf("MaxInput = %d，应等于 MaxOutput 131072（历史语义是输出上限）", e.MaxInput)
	}
	// 上下文仍是输入侧的值
	if e.Context != 1000000 {
		t.Errorf("Context = %d，应为 1000000", e.Context)
	}
}

// 缺 maxOutputTokens 时不能拿输入上限顶替 —— 宁可没有，也不要报错值。
func TestParseWBConfigNoOutputLimitStaysZero(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[
		{"id":"some-model","name":"Some","credits":"x0.1 credits","maxInputTokens":200000}
	]}}`)
	cat, err := parseWBConfigBody(body, "dom")
	if err != nil {
		t.Fatal(err)
	}
	e := cat.byID()["some-model"]
	if e.MaxOutput != 0 {
		t.Errorf("上游没给 maxOutputTokens 时应为 0，实际 %d（不能拿输入上限顶替）", e.MaxOutput)
	}
	if e.MaxInput != 0 {
		t.Errorf("MaxInput 应随 MaxOutput 为 0，实际 %d", e.MaxInput)
	}
	if e.Context != 200000 {
		t.Errorf("Context 应回退到 maxInputTokens 200000，实际 %d", e.Context)
	}
}

// 聚合网关 /v1/models 的 max_output_tokens 不能大于 contextWindow。
func TestAggModelsOutputNotGreaterThanContext(t *testing.T) {
	h := &Hub{}
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.glm-5.3-flash",
		Backend: "glm-5.3-flash", Display: "GLM 5.3 Flash（国内）"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	// 模拟目录已加载：输出 131072、上下文 1000000
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	wbCatalogMu.Lock()
	wbCatalogCache = map[string]*wbCatalog{}
	wbCatalogMu.Unlock()
	t.Cleanup(func() {
		hubRoot = old
		wbCatalogMu.Lock()
		wbCatalogCache = map[string]*wbCatalog{}
		wbCatalogMu.Unlock()
	})
	cat := &wbCatalog{Line: "dom", Entries: []wbCatalogEntry{{
		ID: "glm-5.3-flash", Ratio: "0.06x", Context: 1000000,
		MaxInputTokens: 1000000, MaxOutput: 131072, MaxInput: 131072, Chat: true,
	}}}
	wbCatalogPut(cat)

	resp := h.aggOpenAIModels()
	data, _ := resp["data"].([]map[string]any)
	if len(data) != 1 {
		t.Fatalf("应有 1 个模型，实际 %d", len(data))
	}
	// 注意：map[string]any 里整数是 int（由 Go 构造），但经 json 往返会变 float64。
	// 这里直接读原 map，避免类型陷阱。
	m := data[0]
	out, _ := m["max_output_tokens"].(int)
	ctx, _ := m["contextWindow"].(int)
	if out <= 0 {
		t.Fatalf("max_output_tokens 应 > 0，实际 %v", m["max_output_tokens"])
	}
	if ctx > 0 && out > ctx {
		t.Errorf("max_output_tokens(%d) 大于 contextWindow(%d) —— 输出上限被输入上限污染了", out, ctx)
	}
}
