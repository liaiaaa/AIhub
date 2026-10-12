package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-10-12 修复：qoder/trae 只有 chat/completions 协议，聚合口此前只做了
// 请求侧转换（Responses→chat），响应侧原样返回 chat 格式
// （{"object":"chat.completion","choices":[...]}）。Codex（wire_api=responses）
// 解析不到 output[] 就认为没有内容——实测同一句话经 :8790 返回正文为空，
// 直连 :8797 却正常。这里覆盖响应侧转换。

// fakeChatHandler 模拟 qoder/trae 的 handleChat：吐标准 chat SSE。
func fakeChatHandler(payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		if st, _ := body["stream"].(bool); !st {
			// 聚合口必须强制上游流式，否则 responsesStream 无法逐帧转换
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"聚合口应强制上游 stream=true"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, s := range []string{
			`{"choices":[{"delta":{"reasoning_content":"想想"},"index":0}],"object":"chat.completion.chunk"}`,
			`{"choices":[{"delta":{"content":"你好"}}],"object":"chat.completion.chunk"}`,
			`{"choices":[{"delta":{"content":"啊"}}],"object":"chat.completion.chunk"}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13},"object":"chat.completion.chunk"}`,
		} {
			io.WriteString(w, "data: "+s+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

// 核心断言：经聚合口的 responses 请求必须拿到 Responses 形态（有 output[]），
// 而不是 chat 形态（choices[]）。
func TestAggToChatOnlyReturnsResponsesShape(t *testing.T) {
	up := httptest.NewServer(fakeChatHandler(""))
	defer up.Close()

	p := &Proxy{Name: "qoder", Kind: "qoder", Backend: up.URL, ChatPath: "/v1/chat/completions"}
	// 让 handleChat 直连桩：这里直接用桩的 handler 代替真实 handleChat
	chat := func(w http.ResponseWriter, r *http.Request) {
		// 模拟 handleChat 的行为：把请求转给桩上游并原样回写
		req, _ := http.NewRequest("POST", up.URL, r.Body)
		req.Header = r.Header.Clone()
		resp, err := up.Client().Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
	_ = p

	// 客户端发的是 Responses 请求
	// stream=true：本用例验证的是「流式 Responses 事件转换」
	body := `{"model":"Qwen3.8-Flash","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"说三个字"}]}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	am := aggModel{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"}
	aggToChatOnly(rec, req, "responses", []byte(body), am, chat, "t1", time.Now())

	out := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, truncate(out, 400))
	}
	// 必须是 Responses 的 SSE 事件，而不是 chat.completion 的 JSON
	if !strings.Contains(out, "response.output_text.delta") {
		t.Fatalf("缺少 Responses 文本事件，响应还是 chat 形态？\n%s", truncate(out, 600))
	}
	if !strings.Contains(out, "你好") || !strings.Contains(out, "啊") {
		t.Fatalf("正文丢失：\n%s", truncate(out, 600))
	}
	// 关键回归点：不能把 chat 的 choices 原样漏出去
	if strings.Contains(out, `"object":"chat.completion"`) {
		t.Fatalf("泄漏了 chat 形态：\n%s", truncate(out, 400))
	}
	// 思考链也要转成 Responses 的 reasoning 事件
	if !strings.Contains(out, "reasoning") {
		t.Fatalf("reasoning 未被转换：\n%s", truncate(out, 600))
	}
}

// chat 形态的请求（kind=chat）应直通，不做任何转换
func TestAggToChatOnlyChatPassthrough(t *testing.T) {
	up := httptest.NewServer(fakeChatHandler(""))
	defer up.Close()
	chat := func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("POST", up.URL, r.Body)
		resp, err := up.Client().Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
	body := `{"model":"Qwen3.8-Flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	am := aggModel{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"}
	aggToChatOnly(rec, req, "chat", []byte(body), am, chat, "t2", time.Now())

	out := rec.Body.String()
	if !strings.Contains(out, "chat.completion.chunk") {
		t.Fatalf("chat 请求应原样直通：\n%s", truncate(out, 300))
	}
}

// 上游报错时，错误体与状态码要原样透传（不能被转换吞掉）
func TestAggToChatOnlyErrorPassthrough(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":6004,"msg":"quota"}`)
	}))
	defer up.Close()
	chat := func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("POST", up.URL, r.Body)
		resp, err := up.Client().Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
	body := `{"model":"Qwen3.8-Flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	am := aggModel{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"}
	aggToChatOnly(rec, req, "responses", []byte(body), am, chat, "t3", time.Now())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码应透传 429，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("错误体应透传：%s", rec.Body.String())
	}
}

// realQoderSSE 是 2026-10-12 从 :8797 实抓的 Qoder 流式响应片段
// （Qwen3.8-Flash，含 reasoning 与正文）。用真实帧而非手造帧，
// 避免「测试通过但真实上游形态不同」的假绿。
const realQoderSSE = `data: {"choices":[{"delta":{"content":"","reasoning_content":"","role":"assistant"},"index":0}],"created":1791792400,"id":"chatcmpl-db515c4d-7174-9528-bfa4-e8b54809fad0","model":"auto","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"","reasoning_content":"用户"},"index":0}],"created":1791792400,"id":"chatcmpl-db515c4d-7174-9528-bfa4-e8b54809fad0","model":"auto","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"你好","reasoning_content":""},"index":0}],"created":1791792400,"id":"chatcmpl-db515c4d-7174-9528-bfa4-e8b54809fad0","model":"auto","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"啊","reasoning_content":""},"index":0}],"created":1791792400,"id":"chatcmpl-db515c4d-7174-9528-bfa4-e8b54809fad0","model":"auto","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":"","reasoning_content":""},"finish_reason":"stop","index":0}],"created":1791792400,"id":"chatcmpl-db515c4d-7174-9528-bfa4-e8b54809fad0","model":"auto","object":"chat.completion.chunk"}

data: [DONE]

`

// 用真实抓取的帧验证 chat→Responses 转换：正文不能丢、不能泄漏 chat 形态。
func TestAggToChatOnlyRealQoderFrames(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, realQoderSSE)
	}))
	defer up.Close()

	chat := func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("POST", up.URL, r.Body)
		resp, err := up.Client().Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	body := `{"model":"Qwen3.8-Flash","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"说三个字：你好啊"}]}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	am := aggModel{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"}
	aggToChatOnly(rec, req, "responses", []byte(body), am, chat, "real", time.Now())

	out := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, truncate(out, 400))
	}
	if strings.Contains(out, `"object":"chat.completion"`) {
		t.Fatalf("泄漏了 chat 形态（这正是修复前的 bug）")
	}
	if !strings.Contains(out, "response.output_text.delta") {
		t.Fatalf("缺少 Responses 文本事件：\n%s", truncate(out, 600))
	}
	if !strings.Contains(out, "你好啊") {
		t.Fatalf("正文丢失：\n%s", truncate(out, 600))
	}
	if !strings.Contains(out, "response.completed") {
		t.Fatalf("缺少 response.completed 收尾事件")
	}
}

// 非流式客户端必须拿到一次性 JSON（与 dom/intl 路径的 handleResponses 一致），
// 不能给 SSE——否则客户端 JSON.parse 会炸。这是修「响应侧转换」时差点引入的回归。
func TestAggToChatOnlyNonStreamReturnsJSON(t *testing.T) {
	up := httptest.NewServer(fakeChatHandler(""))
	defer up.Close()
	chat := func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest("POST", up.URL, r.Body)
		resp, err := up.Client().Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	body := `{"model":"Qwen3.8-Flash","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	am := aggModel{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"}
	aggToChatOnly(rec, req, "responses", []byte(body), am, chat, "ns", time.Now())

	out := rec.Body.String()
	if strings.Contains(out, "event: ") || strings.Contains(out, "response.output_text.delta") {
		t.Fatalf("非流式客户端不该收到 SSE：\n%s", truncate(out, 400))
	}
	// 必须是 Responses 形态的 JSON 对象
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("应是合法 JSON：%v\n%s", err, truncate(out, 300))
	}
	if m["object"] != "response" {
		t.Fatalf("object 应为 response，实际 %v", m["object"])
	}
	if _, ok := m["output"]; !ok {
		t.Fatalf("缺少 output 字段（Responses 形态）：%v", m)
	}
	// 正文要在 output 里
	if !strings.Contains(out, "你好") {
		t.Fatalf("正文丢失：\n%s", truncate(out, 400))
	}
}
