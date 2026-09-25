package main

// Qoder 模型目录的单元测试。
//
// 背景（2026-09-23 实测结论）：
//   - Qoder 上游的 model 字段用内部 key（qfmodel / gmodel / dmodel），不是显示名。
//   - 旧代码把默认模型写成显示名 "Qwen3.8-Flash"，并把 qmodel 等当作"已验证模型"，
//     实际漏掉了 qfmodel / qmodel_38max / q37fmodel / dfmodel / gfmodel / gm51model 六个。
//   - 官方目录接口 GET /algo/api/v2/model/list 的 chat 分组与桌面端
//     chat_model_preferences 表给出的 14 个 key 完全一致。
// 本文件锁定这些事实，防止回归。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 内置快照必须覆盖官方 14 个 key，且包含用户关心的 GLM / DeepSeek 系列。
func TestDefaultCatalogCoversAllOfficialKeys(t *testing.T) {
	want := []string{
		"auto", "qmodel_38max", "qfmodel", "qmodel_latest", "qmodel", "q37fmodel",
		"dmodel", "dfmodel", "gmodel", "gfmodel", "gm51model",
		"kmodel_latest", "kmodel", "mmodel",
	}
	got := map[string]qoderModelEntry{}
	for _, m := range defaultQoderCatalog() {
		got[m.Key] = m
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("内置快照缺少官方 key %q", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("内置快照条目数 = %d, want %d（不应多出未验证的 key）", len(got), len(want))
	}

	// 用户明确关心的三家厂商必须都在
	for _, vendor := range []string{"zhipu", "deepseek"} {
		found := false
		for _, m := range got {
			if m.Vendor == vendor {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("内置快照缺少厂商 %q 的模型", vendor)
		}
	}
}

// 显示名必须与官方一致（这些是用户界面上看到的名字）。
func TestDefaultCatalogDisplayNames(t *testing.T) {
	cases := map[string]string{
		"qfmodel":       "Qwen3.8-Flash",
		"dmodel":        "DeepSeek-V4-Pro",
		"dfmodel":       "DeepSeek-Flash",
		"gmodel":        "GLM-5.3",
		"gfmodel":       "GLM-5.3-Flash",
		"gm51model":     "GLM-5.2",
		"kmodel_latest": "Kimi-K3",
		"mmodel":        "MiniMax-M2.7",
	}
	byKey := map[string]string{}
	for _, m := range defaultQoderCatalog() {
		byKey[m.Key] = m.DisplayName
	}
	for k, want := range cases {
		if got := byKey[k]; got != want {
			t.Errorf("%s 显示名 = %q, want %q", k, got, want)
		}
	}
}

// 默认模型必须是真实 key，而不是显示名。
func TestDefaultModelIsRealKey(t *testing.T) {
	if QoderDefaultMd != "qfmodel" {
		t.Fatalf("QoderDefaultMd = %q, want %q（必须是内部 key）", QoderDefaultMd, "qfmodel")
	}
	// 且必须存在于目录中
	found := false
	for _, m := range defaultQoderCatalog() {
		if m.Key == QoderDefaultMd {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("默认模型 %q 不在目录里", QoderDefaultMd)
	}
}

// 别名解析：真实 key、显示名、大小写、下划线/连字符差异都要归一到 key。
func TestResolveModelAliases(t *testing.T) {
	p := NewQoderProxy()
	cases := map[string]string{
		// 真实 key 原样
		"qfmodel":       "qfmodel",
		"gmodel":        "gmodel",
		"dmodel":        "dmodel",
		"qmodel_38max":  "qmodel_38max",
		"kmodel_latest": "kmodel_latest",
		// 官方显示名 → key
		"Qwen3.8-Flash":     "qfmodel",
		"Qwen3.8-Max":       "qmodel_38max",
		"DeepSeek-V4-Pro":   "dmodel",
		"DeepSeek-Flash":    "dfmodel",
		"GLM-5.3":           "gmodel",
		"GLM-5.3-Flash":     "gfmodel",
		"GLM-5.2":           "gm51model",
		"Kimi-K3":           "kmodel_latest",
		"Kimi-K2.8-Preview": "kmodel",
		"MiniMax-M2.7":      "mmodel",
		// 大小写不敏感
		"glm-5.3":         "gmodel",
		"deepseek-v4-pro": "dmodel",
		"QWEN3.8-FLASH":   "qfmodel",
		// 下划线/连字符等价
		"kmodel-latest": "kmodel_latest",
		"qmodel-38max":  "qmodel_38max",
		// 人工别名
		"qwen-3.8-flash": "qfmodel",
		"default":        "qfmodel",
	}
	for in, want := range cases {
		if got := p.resolveModel(in); got != want {
			t.Errorf("resolveModel(%q) = %q, want %q", in, got, want)
		}
	}

	// 空字符串 → 默认 key
	if got := p.resolveModel("   "); got != QoderDefaultMd {
		t.Errorf("resolveModel(空) = %q, want %q", got, QoderDefaultMd)
	}
}

// 未收录的模型名应原样透传（不静默改写），保持对官方上新模型的兼容。
func TestResolveModelUnknownPassesThrough(t *testing.T) {
	p := NewQoderProxy()
	for _, name := range []string{"some-future-model", "brand-new-model-2027"} {
		if got := p.resolveModel(name); got != name {
			t.Errorf("resolveModel(%q) = %q, want 原样透传", name, got)
		}
	}
}

// 请求体缺少 model 字段时，必须落到默认真实 key，而不是字面量 "<nil>"。
func TestHandleChatMissingModelFallsBackToDefault(t *testing.T) {
	p := NewQoderProxy()
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n"))
	}))
	defer srv.Close()

	p.inferURL = srv.URL
	p.client = srv.Client()
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	p.auth = a

	for _, payload := range []string{
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`, // 无 model
		`{"model":null,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		`{"model":"","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	} {
		gotModel = ""
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
		p.handleChat(httptest.NewRecorder(), req)
		if gotModel == "<nil>" {
			t.Errorf("payload %s 把缺失的 model 变成了字面量 <nil>", payload)
		}
		if gotModel != QoderDefaultMd {
			t.Errorf("payload %s → 上游 model = %q, want %q", payload, gotModel, QoderDefaultMd)
		}
	}
}

// 官方目录响应解析：只取 chat 分组，并正确映射字段。
func TestParseQoderModelList(t *testing.T) {
	raw := []byte(`{
		"chat": [
			{"key":"qfmodel","display_name":"Qwen3.8-Flash","format":"openai","source":"system",
			 "enable":true,"is_vl":true,"is_reasoning":true,"is_free":true,"price_factor":0,
			 "max_input_tokens":180000,"context_config":{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true}}},
			{"key":"gmodel","display_name":"GLM-5.3","enable":true,"is_reasoning":true,"price_factor":0.8,"max_input_tokens":180000},
			{"key":"gone","display_name":"Retired","enable":false}
		],
		"developer": [{"key":"should-be-ignored","display_name":"X","enable":true}]
	}`)

	entries, err := parseQoderModelList(raw)
	if err != nil {
		t.Fatalf("parseQoderModelList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("条目数 = %d, want 2（应只取 chat 且过滤 enable=false）", len(entries))
	}
	if entries[0].Key != "qfmodel" || entries[0].DisplayName != "Qwen3.8-Flash" {
		t.Errorf("首条 = %+v", entries[0])
	}
	if entries[0].Vendor != "alibaba" {
		t.Errorf("qfmodel 厂商 = %q, want alibaba", entries[0].Vendor)
	}
	if !entries[0].IsFree || !entries[0].IsReasoning || !entries[0].IsVision {
		t.Errorf("qfmodel 能力标记解析错误：%+v", entries[0])
	}
	if entries[1].Vendor != "zhipu" {
		t.Errorf("gmodel 厂商 = %q, want zhipu", entries[1].Vendor)
	}
	for _, e := range entries {
		if e.Key == "should-be-ignored" || e.Key == "gone" {
			t.Errorf("不应包含 %q", e.Key)
		}
	}
}

// chat 分组缺失时应回退到其他非空分组，而不是直接失败。
func TestParseQoderModelListFallbackGroup(t *testing.T) {
	raw := []byte(`{"someOtherGroup":[{"key":"k1","display_name":"M1","enable":true}]}`)
	entries, err := parseQoderModelList(raw)
	if err != nil {
		t.Fatalf("parseQoderModelList: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "k1" {
		t.Fatalf("回退解析结果 = %+v", entries)
	}
}

// 非法输入应报错而非 panic。
func TestParseQoderModelListErrors(t *testing.T) {
	for _, raw := range []string{`not json`, `{}`, `{"chat":[]}`} {
		if _, err := parseQoderModelList([]byte(raw)); err == nil {
			t.Errorf("parseQoderModelList(%q) 应返回错误", raw)
		}
	}
}

// 动态刷新成功后，目录与别名表都要热替换（新模型立即可用）。
func TestSetCatalogHotSwap(t *testing.T) {
	p := NewQoderProxy()
	p.setCatalog([]qoderModelEntry{
		{Key: "newmodel", DisplayName: "Brand-New-2027", Vendor: "qoder-cn"},
	})
	if got := p.resolveModel("Brand-New-2027"); got != "newmodel" {
		t.Errorf("刷新后别名解析 = %q, want newmodel", got)
	}
	if got := p.resolveModel("newmodel"); got != "newmodel" {
		t.Errorf("刷新后 key 解析 = %q, want newmodel", got)
	}
	// 空目录不应清空现有目录
	p.setCatalog(nil)
	if got := p.resolveModel("newmodel"); got != "newmodel" {
		t.Errorf("空目录不应清空现有目录，得到 %q", got)
	}
}

// /v1/models 每个模型只出现一条（用官方显示名做 id），不能有重复项。
func TestHandleModelsNoDuplicates(t *testing.T) {
	p := NewQoderProxy()
	rec := httptest.NewRecorder()
	p.handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Object     string `json:"object"`
		Default    string `json:"default"`
		DefaultKey string `json:"default_key"`
		Data       []struct {
			ID         string `json:"id"`
			OwnedBy    string `json:"owned_by"`
			ResolvesTo string `json:"resolves_to"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败：%v\n%s", err, rec.Body.String())
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}

	// 14 个模型 → 恰好 14 条，且 id 互不相同（这是本次修复的核心诉求）
	if len(resp.Data) != 14 {
		t.Errorf("条目数 = %d, want 14（一个模型只许一条，不许重复）", len(resp.Data))
	}
	ids := map[string]int{}
	for _, d := range resp.Data {
		ids[d.ID]++
		if ids[d.ID] > 1 {
			t.Errorf("模型 id %q 出现了 %d 次（不应重复）", d.ID, ids[d.ID])
		}
	}

	// 显示名必须都在，且恰好一次
	for _, want := range []string{
		"Auto", "Qwen3.8-Max", "Qwen3.8-Flash", "Qwen3.7-Max", "Qwen3.7-Plus", "Qwen3.7-Flash",
		"DeepSeek-V4-Pro", "DeepSeek-Flash", "GLM-5.3", "GLM-5.3-Flash", "GLM-5.2",
		"Kimi-K3", "Kimi-K2.8-Preview", "MiniMax-M2.7",
	} {
		if ids[want] != 1 {
			t.Errorf("缺少或重复 %q（出现 %d 次）", want, ids[want])
		}
	}
	// 真实 key 不应再作为独立条目出现（避免与显示名成对重复）
	for _, key := range []string{"qfmodel", "gmodel", "dmodel", "qmodel_38max"} {
		if ids[key] != 0 {
			t.Errorf("真实 key %q 不应再单独成条（会造成重复）", key)
		}
	}

	// default 必须是列表里真实存在的 id，且 default_key 指向真实 key
	if ids[resp.Default] != 1 {
		t.Errorf("default %q 不在列表里", resp.Default)
	}
	if resp.DefaultKey != "qfmodel" {
		t.Errorf("default_key = %q, want qfmodel", resp.DefaultKey)
	}
	for _, d := range resp.Data {
		if d.ID == "GLM-5.3" && d.ResolvesTo != "gmodel" {
			t.Errorf("GLM-5.3 的 resolves_to = %q, want gmodel", d.ResolvesTo)
		}
	}
}

// 动态目录万一出现重名显示名：不能产生重复 id，真实 key 仍可直接调用。
func TestHandleModelsDuplicateDisplayName(t *testing.T) {
	p := NewQoderProxy()
	p.setCatalog([]qoderModelEntry{
		{Key: "aaa", DisplayName: "Same-Name", Vendor: "qoder-cn"},
		{Key: "bbb", DisplayName: "Same-Name", Vendor: "qoder-cn"},
	})
	rec := httptest.NewRecorder()
	p.handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp struct {
		Data []struct {
			ID         string `json:"id"`
			ResolvesTo string `json:"resolves_to"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "Same-Name" || resp.Data[0].ResolvesTo != "aaa" {
		t.Fatalf("重名时应只保留一条 = %+v", resp.Data)
	}
	// 两个 key 都仍可直接调用（别名映射不受列表去重影响）
	if got := p.resolveModel("aaa"); got != "aaa" {
		t.Errorf("resolveModel(aaa) = %q", got)
	}
	if got := p.resolveModel("bbb"); got != "bbb" {
		t.Errorf("resolveModel(bbb) = %q", got)
	}
}

// 动态拉取失败（如网络不通 / 非 200）必须返回错误，让调用方保留内置快照。
func TestFetchQoderCatalogErrors(t *testing.T) {
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}

	// 500 响应
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fetchQoderCatalog(t.Context(), a, srv.Client(), srv.URL); err == nil {
		t.Error("HTTP 500 应返回错误")
	}

	// 正常 200 + 合法目录
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		// COSY 头必须齐备
		for _, h := range []string{"Authorization", "Cosy-User", "Cosy-Key", "Cosy-Date"} {
			if r.Header.Get(h) == "" {
				t.Errorf("缺少请求头 %s", h)
			}
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
			t.Errorf("Authorization 格式错误：%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"chat":[{"key":"x1","display_name":"X-One","enable":true}]}`))
	}))
	defer ok.Close()
	entries, err := fetchQoderCatalog(t.Context(), a, ok.Client(), ok.URL)
	if err != nil {
		t.Fatalf("fetchQoderCatalog: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "x1" {
		t.Fatalf("解析结果 = %+v", entries)
	}
}

// 端到端：默认情况下发到上游的请求必须带真实思考参数（parameters.reasoning_effort，
// 取目录默认档），且不得注入任何提示词 hack。
func TestHandleChatSendsThinkingParamsUpstream(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n"))
	}))
	defer srv.Close()

	p := NewQoderProxy()
	p.inferURL = srv.URL
	p.client = srv.Client()
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	p.auth = a

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	p.handleChat(httptest.NewRecorder(), req)

	if gotBody["model"] != "gmodel" {
		t.Errorf("上游 model = %v, want gmodel", gotBody["model"])
	}
	// 真实思考旋钮：parameters.reasoning_effort（gmodel 目录默认档 = max）
	params, _ := gotBody["parameters"].(map[string]any)
	if got := params["reasoning_effort"]; got != "max" {
		t.Errorf("上游 parameters.reasoning_effort = %v, want max（目录默认档）", got)
	}
	// system 只允许温和思考提示（保护输出格式），严禁刚性措辞
	msgs := gotBody["messages"].([]any)
	all := ""
	for _, m := range msgs {
		if c, _ := m.(map[string]any)["content"].(string); c != "" {
			all += c + "\n"
		}
	}
	if strings.Contains(all, "缺一不可") || strings.Contains(all, "严格按以下两部分") {
		t.Errorf("禁止刚性思考措辞上行，实际 %q", all)
	}
	if !strings.Contains(all, "不要把思考内容混入其中") {
		t.Errorf("应注入温和思考提示（含“不要把思考内容混入其中”），实际 %q", all)
	}
	// 无效的 thinking 字段不应发给上游（翻译进 parameters 后从顶层删除）
	if _, exists := gotBody["thinking"]; exists {
		t.Errorf("不应向上游透传 thinking 字段")
	}
	if _, exists := gotBody["reasoning_effort"]; exists {
		t.Errorf("顶层 reasoning_effort 应翻译进 parameters 后删除")
	}
}
