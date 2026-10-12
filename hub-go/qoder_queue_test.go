package main

// 排队帧（10605）识别与队列感知重试测试。
// 背景：2026-09-26 晚高峰网关返回 HTTP 200 + 外层 code=403 的排队帧，
// 旧路径当 provider_error 只重试 2 次×700ms 后，把「上游网关错误 403: …」
// 透传给 DSH，客户端误判为「API 密钥无效」——凭据其实完全正常。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 观测到的真实排队帧（session-446ef8d8，2026-09-26 23:29，qfmodel）。
const qoderQueueFrameObserved = `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":8445,\\\"queueType\\\":\\\"p3\\\",\\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":true,\\\"waitTime\\\":261}\"}"}`

func TestParseQueueFrame(t *testing.T) {
	qi, ok := parseQueueFrame(qoderQueueFrameObserved)
	if !ok {
		t.Fatal("观测排队帧未被识别")
	}
	if qi.Code != "10605" || qi.QueueCount != 8445 || qi.QueueType != "p3" ||
		qi.RetryAfterSeconds != 30 || qi.WaitTime != 261 || !qi.ServiceAvailable {
		t.Fatalf("字段解析不符: %+v", qi)
	}

	// 信封包装 + 多层嵌套（程序化构造，避免手工数反斜杠）
	inner := `{"isQueued":true,"queueCount":7,"retryAfterSeconds":30,"serviceAvailable":true,"waitTime":90}`
	mid := fmt.Sprintf(`{"code":"10605","message":%s}`, strconv.Quote(inner))
	outer := fmt.Sprintf(`{"code":"403","message":%s}`, strconv.Quote(mid))
	wrapped := fmt.Sprintf(`{"headers":{},"statusCodeValue":200,"body":%s}`, strconv.Quote(outer))
	qi, ok = parseQueueFrame(wrapped)
	if !ok || qi.QueueCount != 7 || qi.WaitTime != 90 || !qi.ServiceAvailable {
		t.Fatalf("包装帧解析不符: ok=%v qi=%+v", ok, qi)
	}

	// 裸 10605 帧（无外层 403）
	qi, ok = parseQueueFrame(`{"code":"10605","message":"{\"isQueued\":true,\"queueCount\":100,\"retryAfterSeconds\":5,\"serviceAvailable\":true,\"waitTime\":60}"}`)
	if !ok || qi.QueueCount != 100 || qi.RetryAfterSeconds != 5 {
		t.Fatalf("裸帧解析不符: ok=%v qi=%+v", ok, qi)
	}

	// serviceAvailable=false 明示不可用
	qi, ok = parseQueueFrame(`{"code":"10605","message":"{\"isQueued\":true,\"queueCount\":5,\"serviceAvailable\":false}"}`)
	if !ok || qi.serviceUp() {
		t.Fatalf("serviceAvailable=false 未识别: ok=%v qi=%+v", ok, qi)
	}

	// 非排队帧一律不误判
	for _, bad := range []string{
		`{"code":"provider_error","message":"Error in upstream response"}`,
		`{"code":"400","message":"[FAIL]node:oa_qwen-plus-main msg:Execution failed"}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`[DONE]`,
		`not json`,
		``,
	} {
		if _, ok := parseQueueFrame(bad); ok {
			t.Errorf("误判为排队帧: %q", bad)
		}
	}
}

func TestFirstFrameQueueInfo(t *testing.T) {
	// 排队帧前有心跳/空壳行也应识别
	head := "event: ping\ndata: {}\n\ndata: " + qoderQueueFrameObserved + "\n\n"
	qi, ok := firstFrameQueueInfo([]byte(head))
	if !ok || qi.QueueCount != 8445 {
		t.Fatalf("firstFrameQueueInfo: ok=%v qi=%+v", ok, qi)
	}
	// 正常 chunk 先行 → 非排队
	if _, ok := firstFrameQueueInfo([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")); ok {
		t.Fatal("正常帧被误判为排队帧")
	}
}

func TestQueueErrorMessageNo403(t *testing.T) {
	qi, _ := parseQueueFrame(qoderQueueFrameObserved)
	msg := (&qoderQueueError{Info: qi, Waited: 240 * time.Second}).Error()
	if strings.Contains(msg, "403") || strings.Contains(msg, "密钥") {
		t.Fatalf("放弃文案仍含 403/密钥字样: %s", msg)
	}
	if !strings.Contains(msg, "排队") || !strings.Contains(msg, "429") {
		t.Fatalf("放弃文案缺排队/429 语义: %s", msg)
	}
	// 流中途兜底（upstreamErrorFrame 改写）同样不得含 403
	msg, ok := upstreamErrorFrame(qoderQueueFrameObserved)
	if !ok || strings.Contains(msg, "403") || !strings.Contains(msg, "排队") {
		t.Fatalf("upstreamErrorFrame 排队改写不符: ok=%v msg=%s", ok, msg)
	}
}

func TestQoderQueueNextWait(t *testing.T) {
	ctx := context.Background()
	// serviceAvailable=false → 立即放弃
	qi, _ := parseQueueFrame(`{"code":"10605","message":"{\"isQueued\":true,\"serviceAvailable\":false}"}`)
	if _, giveup := qoderQueueNextWait(ctx, qi, 0, time.Minute); !giveup {
		t.Fatal("服务不可用时应放弃重试")
	}
	// 预算关闭 → 放弃
	qi, _ = parseQueueFrame(`{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\":30}"}`)
	if _, giveup := qoderQueueNextWait(ctx, qi, 0, 0); !giveup {
		t.Fatal("预算为 0 时应放弃重试")
	}
	// 超预算 → 放弃
	if _, giveup := qoderQueueNextWait(ctx, qi, 240*time.Second, 240*time.Second); !giveup {
		t.Fatal("已等满预算后应放弃")
	}
	// 正常：等建议间隔
	d, giveup := qoderQueueNextWait(ctx, qi, 0, 240*time.Second)
	if giveup || d != 30*time.Second {
		t.Fatalf("wait = %v giveup = %v, want 30s/false", d, giveup)
	}
	// 建议间隔超 60s 时封顶
	qi, _ = parseQueueFrame(`{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\":300}"}`)
	if d, _ := qoderQueueNextWait(ctx, qi, 0, 10*time.Minute); d != time.Minute {
		t.Fatalf("封顶失败: %v", d)
	}
}

// 队列帧按建议间隔真实重试，第三次拿到正常流（预算 3s，间隔 1s）。
func TestOpenInferValidatedQueueRetry(t *testing.T) {
	t.Setenv(qoderQueueWaitEnv, "3")
	queueFrame := `{"code":"10605","message":"{\"isQueued\":true,\"queueCount\":3706,\"retryAfterSeconds\":1,\"serviceAvailable\":true,\"waitTime\":115}"}`
	upstream := "data: {\"headers\":{},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n" +
		"data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls <= 2 {
			_, _ = w.Write([]byte("data: " + queueFrame + "\n\n"))
			return
		}
		_, _ = w.Write([]byte(upstream))
	}))
	defer srv.Close()

	p := newTestQoderProxy(t, srv.URL)
	start := time.Now()
	resp, err := p.openInferValidated(context.Background(), []byte(`{}`), nil, "q01", nil)
	if err != nil {
		t.Fatalf("openInferValidated: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != upstream {
		t.Fatalf("排队重试后流内容被破坏:\n got  = %q\n want = %q", got, upstream)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3（两次排队重试后成功）", calls)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("未按建议间隔真实等待: elapsed = %v", elapsed)
	}
}

// 预算耗尽仍排队 → 放弃，返回 429 语义错误，文案无 403/密钥。
func TestOpenInferValidatedQueueGiveUp(t *testing.T) {
	t.Setenv(qoderQueueWaitEnv, "2")
	queueFrame := `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"queueCount\\\":8445,\\\"queueType\\\":\\\"p3\\\",\\\"retryAfterSeconds\\\":2,\\\"serviceAvailable\\\":true,\\\"waitTime\\\":261}\"}"}`
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + queueFrame + "\n\n"))
	}))
	defer srv.Close()

	p := newTestQoderProxy(t, srv.URL)
	_, err := p.openInferValidated(context.Background(), []byte(`{}`), nil, "q02", nil)
	if err == nil {
		t.Fatal("预算耗尽应返回错误")
	}
	var qe *qoderQueueError
	if !asQueueError(err, &qe) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	if qe.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("HTTPStatus = %d, want 429", qe.HTTPStatus())
	}
	if msg := qe.Error(); strings.Contains(msg, "403") || strings.Contains(msg, "密钥") || !strings.Contains(msg, "排队") {
		t.Fatalf("放弃文案误导: %s", msg)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2（预算 2s 只够一次 2s 等待）", calls)
	}
}

// 预算关闭（=0）→ 不等待直接放弃。
func TestOpenInferValidatedQueueDisabled(t *testing.T) {
	t.Setenv(qoderQueueWaitEnv, "0")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + qoderQueueFrameObserved + "\n\n"))
	}))
	defer srv.Close()

	p := newTestQoderProxy(t, srv.URL)
	_, err := p.openInferValidated(context.Background(), []byte(`{}`), nil, "q03", nil)
	if err == nil {
		t.Fatal("预算关闭应返回错误")
	}
	var qe *qoderQueueError
	if !asQueueError(err, &qe) {
		t.Fatalf("错误类型不符: %T %v", err, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1（关闭预算不应重试）", calls)
	}
}

// HTTP 403 + 10605 排队体的兜底形态也要走队列重试。
func TestOpenInferValidatedQueueHTTP403Body(t *testing.T) {
	t.Setenv(qoderQueueWaitEnv, "2")
	queueBody := `{"code":"10605","message":"{\"isQueued\":true,\"queueCount\":100,\"retryAfterSeconds\":1,\"serviceAvailable\":true,\"waitTime\":60}"}`
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(queueBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n"))
	}))
	defer srv.Close()

	p := newTestQoderProxy(t, srv.URL)
	resp, err := p.openInferValidated(context.Background(), []byte(`{}`), nil, "q04", nil)
	if err != nil {
		t.Fatalf("HTTP 403 排队体应触发重试: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d, want 200/2", resp.StatusCode, calls)
	}
}

// 非排队的普通 403 不受影响，原样交回上层。
func TestOpenInferValidatedPlain403PassThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"403","message":"Forbidden"}`))
	}))
	defer srv.Close()

	p := newTestQoderProxy(t, srv.URL)
	resp, err := p.openInferValidated(context.Background(), []byte(`{}`), nil, "q05", nil)
	if err != nil {
		t.Fatalf("openInferValidated: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 原样透传", resp.StatusCode)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || m["code"] != "403" {
		t.Fatalf("body 被破坏: %v %v", err, m)
	}
}

// --- 测试辅助 ---

func newTestQoderProxy(t *testing.T, url string) *QoderProxy {
	t.Helper()
	p := NewQoderProxy()
	p.inferURL = url
	p.client = &http.Client{}
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	p.auth = a
	return p
}

func asQueueError(err error, target **qoderQueueError) bool {
	if qe, ok := err.(*qoderQueueError); ok {
		*target = qe
		return true
	}
	return false
}
