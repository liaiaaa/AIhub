package main

// Zcode 代理 handler 级测试：httptest 假上游，锁定请求改写与模型清单。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestZcodeHandleChatUpstreamBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer zk-test") {
			t.Errorf("Authorization = %q, want Bearer zk-test...", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewZcodeProxy()
	p.Endpoint = srv.URL
	p.APIKey = "zk-test-key"
	p.apiKey = p.APIKey
	p.client = srv.Client()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[{"role":"user","content":"[no-thinking] hi"}],"stream":true,"verbosity":"low"}`))
	p.handleChat(httptest.NewRecorder(), req)

	// 别名 → 真实 key；流式强制开启
	if gotBody["model"] != "glm-5.3" {
		t.Errorf("上游 model = %v, want glm-5.3（显示名映射）", gotBody["model"])
	}
	if gotBody["stream"] != true {
		t.Errorf("上游 stream = %v, want true（强制流式）", gotBody["stream"])
	}
	// [no-thinking] 标记摘除 + 5.3 系关闭 → low + enabled
	th, _ := gotBody["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Errorf("上游 thinking = %v, want type=enabled", gotBody["thinking"])
	}
	if gotBody["reasoning_effort"] != "low" {
		t.Errorf("上游 reasoning_effort = %v, want low（5.3 系不可关闭）", gotBody["reasoning_effort"])
	}
	// 标记与 OpenAI 专有字段不透传
	msgs := gotBody["messages"].([]any)
	if c, _ := msgs[0].(map[string]any)["content"].(string); strings.Contains(c, "[no-thinking]") {
		t.Errorf("标记应被摘除, 实际 %q", c)
	}
	if _, exists := gotBody["verbosity"]; exists {
		t.Errorf("verbosity 不应透传")
	}
}

// /v1/models：显示名 id + 能力元数据（视觉/思考/上下文/输出）。
func TestZcodeHandleModels(t *testing.T) {
	p := NewZcodeProxy()
	rec := httptest.NewRecorder()
	p.handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))

	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 /v1/models: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, m := range out.Data {
		byID[m["id"].(string)] = m
	}
	glm, ok := byID["GLM-5.3"]
	if !ok {
		t.Fatalf("缺少 GLM-5.3: %v", out.Data)
	}
	if glm["resolves_to"] != "glm-5.3" {
		t.Errorf("resolves_to = %v, want glm-5.3", glm["resolves_to"])
	}
	if glm["vision"] != false {
		t.Errorf("GLM-5.3 vision = %v, want false（纯文本）", glm["vision"])
	}
	flash, ok := byID["GLM-5.3-Flash"]
	if !ok {
		t.Fatalf("缺少 GLM-5.3-Flash: %v", out.Data)
	}
	if flash["vision"] != true {
		t.Errorf("GLM-5.3-Flash vision = %v, want true", flash["vision"])
	}
	th, _ := flash["thinking"].(map[string]any)
	if th["can_disable"] != false {
		t.Errorf("flash 思考不可关闭, 实际 %v", th["can_disable"])
	}
}

// 未知模型原样透传给上游。
func TestZcodeHandleChatUnknownModelPassthrough(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewZcodeProxy()
	p.Endpoint = srv.URL
	p.APIKey = "zk-test-key"
	p.apiKey = p.APIKey
	p.client = srv.Client()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-9.9-turbo","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	p.handleChat(httptest.NewRecorder(), req)
	if gotBody["model"] != "glm-9.9-turbo" {
		t.Errorf("未知模型应原样透传, 实际 %v", gotBody["model"])
	}
}
