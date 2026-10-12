package main

// 回归测试：上游以「网关业务错误帧」形式返回失败时（HTTP 200，但首帧是
// {"code":"400","message":...}），代理必须让客户端明确看到错误，而不是
// 静默返回一个空流。
//
// 背景（2026-09-25 实测）：Qoder 网关在某些 tools 形态/配额波动时会返回
//
//	data: {"code":"400","message":"[FAIL]node:oa_qwen-plus-main msg:Execution failed: ..."}
//
// 而 streamToClient 在读到任何上游数据之前就已经 WriteHeader(200)，
// 于是这个错误帧被当成普通 SSE 数据转发，客户端最终只看到「模型没有输出」，
// 完全不知道真实原因。本测试锁定该行为。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// zzNewErrFrameProxy 构造一个上游：HTTP 200，但首帧就是网关错误帧。
func zzNewErrFrameProxy(t *testing.T, errFrame string) *QoderProxy {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(errFrame))
	}))
	t.Cleanup(srv.Close)

	p := NewQoderProxy()
	p.inferURL = srv.URL
	p.client = srv.Client()
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	p.auth = a
	return p
}

// TestUpstreamErrorFrameSurfacesToClient 是本 bug 的核心回归测试：
// 上游错误帧必须让客户端看到错误，而不是静默空流。
func TestUpstreamErrorFrameSurfacesToClient(t *testing.T) {
	// 真实网关错误帧形态（节选自实测日志）
	errFrame := "data: {\"code\":\"400\",\"message\":\"[FAIL]node:oa_qwen-plus-main msg:Execution failed: java.util.LinkedHashMap cannot be cast to java.util.List\"}\n\n"

	p := zzNewErrFrameProxy(t, errFrame)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))

	p.streamToClient(rec, req, []byte(`{"model":"qfmodel","messages":[],"stream":true}`), time.Now(), "t-err")

	body := rec.Body.String()
	t.Logf("HTTP=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	t.Logf("body=%q", body)

	// 断言 1：必须转写成标准 OpenAI error 形态（客户端才会显示）
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("上游错误帧未转写成标准 OpenAI error，客户端会静默忽略\nbody=%q", body)
	}
	// 断言 2：错误原文必须可见（不能只给笼统提示，否则无法定位）
	if !strings.Contains(body, "cannot be cast") {
		t.Fatalf("错误原文丢失，用户无法定位真实原因\nbody=%q", body)
	}
	// 断言 3：不应再出现裸错误帧（无 error 包装的 code/message）
	if strings.Contains(body, `data: {"code"`) {
		t.Fatalf("仍在转发裸错误帧（客户端会忽略）\nbody=%q", body)
	}
}

// TestNormalStreamNotAffectedByErrorHandling 确保正常流不被错误识别逻辑误伤。
func TestNormalStreamNotAffectedByErrorHandling(t *testing.T) {
	upstream := "data: {\"headers\":{},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"Hi\\\"}}]}\"}\n\n" +
		"data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n"

	p := zzNewErrFrameProxy(t, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))
	p.streamToClient(rec, req, []byte(`{"model":"qfmodel","messages":[],"stream":true}`), time.Now(), "t-ok")

	body := rec.Body.String()
	t.Logf("body=%q", body)
	if !strings.Contains(body, `"content":"Hi"`) {
		t.Fatalf("正常内容帧被误伤\nbody=%q", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("正常流不应出现 error 事件\nbody=%q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("缺少 [DONE] 结束帧\nbody=%q", body)
	}
}

// TestUpstreamErrorFrameDetector 单测错误帧识别函数本身。
func TestUpstreamErrorFrameDetector(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantOK bool
	}{
		{"网关业务错误帧", `{"code":"400","message":"[FAIL]...Execution failed"}`, true},
		{"只有 message", `{"message":"boom"}`, true},
		{"正常 choices 帧", `{"choices":[{"delta":{"content":"hi"}}]}`, false},
		{"已是标准 error", `{"error":{"message":"x","type":"y"}}`, false},
		{"DONE", `[DONE]`, false},
		{"空串", ``, false},
		{"非 JSON", `not json`, false},
	}
	for _, c := range cases {
		msg, ok := upstreamErrorFrame(c.in)
		if ok != c.wantOK {
			t.Errorf("[%s] ok=%v want=%v (msg=%q)", c.name, ok, c.wantOK, msg)
			continue
		}
		if ok && msg == "" {
			t.Errorf("[%s] 识别为错误帧但文案为空", c.name)
		}
	}
}
