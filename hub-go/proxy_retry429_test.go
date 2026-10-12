package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- planRetry429：两种 429 必须被区分开 ----

// 14003（retry_policy.retry=true）→ 可重试
func TestPlan429GatewaySaysRetry(t *testing.T) {
	body := []byte(`{"code":14003,"msg":"too many requests","retry_policy":{"retry":true},` +
		`"displayMsg":{"en":"Too many requests. Please retry later.","zh":"请求过于频繁"}}`)
	p := planRetry429(http.Header{}, body, 1)
	if !p.retryable {
		t.Fatalf("网关明示可重试，却被判为不可重试：%+v", p)
	}
	if p.wait <= 0 {
		t.Fatalf("重试等待应为正值：%v", p.wait)
	}
}

// 6004（配额耗尽，文案带恢复时刻）→ 不可重试，且要说清几点恢复
func TestPlan429QuotaExhaustedNotRetryable(t *testing.T) {
	body := []byte(`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, ` +
		`your usage will reset at 2030-01-01 10:00:00 UTC+8, alternatively, you can switch to the other models"}`)
	p := planRetry429(http.Header{}, body, 1)
	if p.retryable {
		t.Fatalf("配额耗尽不该重试（等到 2030 年毫无意义）：%+v", p)
	}
	if !strings.Contains(p.hint, "2030-01-01") {
		t.Fatalf("提示里必须写清恢复时刻：%q", p.hint)
	}
	if !strings.Contains(p.hint, "换凭证") {
		t.Fatalf("提示里应给出可操作建议：%q", p.hint)
	}
}

// 恢复时刻已过 → 按瞬时限流处理（可重试）
func TestPlan429QuotaResetAlreadyPassed(t *testing.T) {
	body := []byte(`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, ` +
		`your usage will reset at 2020-01-01 10:00:00 UTC+8, alternatively, you can switch to the other models"}`)
	p := planRetry429(http.Header{}, body, 1)
	if !p.retryable {
		t.Fatalf("恢复时刻已过，应可重试：%+v", p)
	}
}

// Retry-After 头最权威，且不被硬砍（上游说 30s 就是 30s）
func TestPlan429RetryAfterHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	p := planRetry429(h, []byte(`{"code":14003}`), 1)
	if !p.retryable || p.wait != 30*time.Second {
		t.Fatalf("Retry-After 未被尊重：%+v", p)
	}
}

// 裸 429（无 body/无头）→ 按标准语义保守重试
func TestPlan429BareIsRetryable(t *testing.T) {
	if p := planRetry429(http.Header{}, []byte(""), 1); !p.retryable {
		t.Fatalf("裸 429 应保守重试：%+v", p)
	}
}

// 退避序列应递增且有上限
func TestBackoff429GrowsAndCaps(t *testing.T) {
	var prev time.Duration
	for i := 1; i <= 8; i++ {
		w := backoff429(i)
		if w <= 0 {
			t.Fatalf("attempt=%d 等待非正：%v", i, w)
		}
		if i >= 6 && w > retry429MaxSingleWait+retry429MaxSingleWait/2 {
			t.Fatalf("attempt=%d 超过单次上限太多：%v", i, w)
		}
		if i <= 5 && w < prev/2 {
			t.Fatalf("attempt=%d 退避没有增长趋势：%v < %v", i, w, prev)
		}
		prev = w
	}
}

// ---- 端到端：退避重试真的把 429 吸收掉 ----

// 上游前两次回 429（可重试），第三次成功 → 客户端应拿到 200，无感。
func TestRetry429AbsorbsTransientLimit(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0") // 0 秒，测试跑得快
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":14003,"msg":"too many requests","retry_policy":{"retry":true}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := &Proxy{Name: "intl", Kind: "cred"}
	logger := NewFileLogger("")
	resp, err := p.doUpstreamRetry429(context.Background(), up.Client(), staticAuth{}, up.URL, nil, []byte("{}"), logger, "t1")
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("429 未被吸收，状态=%d body=%s", resp.StatusCode, b)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("应重试到第 3 次成功，实际调用 %d 次", got)
	}
}

// 配额耗尽型（6004）→ 不该反复打上游，且错误体里带人话
func TestRetry429QuotaFailsFastWithHint(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at 2030-01-01 10:00:00 UTC+8","displayMsg":{"zh":"配额超限","en":"limit"}}`)
	}))
	defer up.Close()

	p := &Proxy{Name: "intl", Kind: "cred"}
	resp, err := p.doUpstreamRetry429(context.Background(), up.Client(), staticAuth{}, up.URL, nil, []byte("{}"), NewFileLogger(""), "t2")
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("配额耗尽应原样返回 429，实际 %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("配额耗尽不该重试，实际打了 %d 次上游", got)
	}
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("错误体应是合法 JSON：%s", b)
	}
	dm, _ := m["displayMsg"].(map[string]any)
	zh, _ := dm["zh"].(string)
	if !strings.Contains(zh, "2030-01-01") {
		t.Fatalf("应把恢复时刻写进 zh 提示：%q", zh)
	}
}

// 预算耗尽 → 返回 429，但带「已重试 N 次」的人话；且总耗时受预算约束
func TestRetry429BudgetExhaustedReportsAttempts(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":14003,"msg":"too many requests","retry_policy":{"retry":true},"displayMsg":{"zh":"频繁","en":"busy"}}`)
	}))
	defer up.Close()

	t.Setenv(retry429BudgetEnv, "2") // 2 秒预算
	p := &Proxy{Name: "intl", Kind: "cred"}
	t0 := time.Now()
	resp, err := p.doUpstreamRetry429(context.Background(), up.Client(), staticAuth{}, up.URL, nil, []byte("{}"), NewFileLogger(""), "t3")
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("预算耗尽应返回 429，实际 %d", resp.StatusCode)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("预算 2s 却耗时 %v，退避没受预算约束", elapsed)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "[hub]") {
		t.Fatalf("应附上 hub 说明：%s", b)
	}
	if !strings.Contains(string(b), "退避重试") {
		t.Fatalf("应说明已重试过：%s", b)
	}
}

// 关闭开关（预算=0）→ 完全退回原行为（只打一次，原样透传）
func TestRetry429DisabledPassesThrough(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":14003}`)
	}))
	defer up.Close()

	t.Setenv(retry429BudgetEnv, "0")
	p := &Proxy{Name: "intl", Kind: "cred"}
	resp, err := p.doUpstreamRetry429(context.Background(), up.Client(), staticAuth{}, up.URL, nil, []byte("{}"), NewFileLogger(""), "t4")
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("关闭后不该重试，实际 %d 次", got)
	}
}

// 非 429（如 500）不该被这个包装碰
func TestRetry429IgnoresOtherStatuses(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer up.Close()

	p := &Proxy{Name: "intl", Kind: "cred"}
	resp, err := p.doUpstreamRetry429(context.Background(), up.Client(), staticAuth{}, up.URL, nil, []byte("{}"), NewFileLogger(""), "t5")
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("500 被误处理：status=%d calls=%d", resp.StatusCode, calls)
	}
}

// ctx 取消（客户端断开）时应尽快返回，不空等预算
func TestRetry429AbortsOnContextCancel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":14003,"retry_policy":{"retry":true}}`)
	}))
	defer up.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()

	p := &Proxy{Name: "intl", Kind: "cred"}
	t0 := time.Now()
	resp, err := p.doUpstreamRetry429(ctx, up.Client(), staticAuth{}, up.URL, nil, []byte("{}"), NewFileLogger(""), "t6")
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	defer resp.Body.Close()
	if elapsed > 5*time.Second {
		t.Fatalf("ctx 已取消却等了 %v", elapsed)
	}
}

// 放弃重试时 body 被加长，Content-Length 必须重算（否则 header 与 body 不符）
func TestSynthResponseRecalculatesLength(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Length", "15")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Content-Encoding", "gzip")
	h.Set("X-Upstream", "keep-me") // 非传输层的头应保留
	body := []byte(`{"code":14003}` + "[hub] 追加的中文说明，让 body 变长")
	r := synthResponse(http.StatusTooManyRequests, h, body)

	if got := r.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length 未重算：%s，实际 body %d", got, len(body))
	}
	if r.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength 字段未同步：%d", r.ContentLength)
	}
	for _, bad := range []string{"Transfer-Encoding", "Content-Encoding"} {
		if r.Header.Get(bad) != "" {
			t.Fatalf("传输层头 %s 应被剔除（body 已被我们改写/解压）", bad)
		}
	}
	if r.Header.Get("X-Upstream") != "keep-me" {
		t.Fatalf("普通头不该被丢：%v", r.Header)
	}
	if !r.Uncompressed {
		t.Fatalf("Uncompressed 应为 true（body 是明文）")
	}
}

// staticAuth 测试用最小 AuthProvider（401 自愈路径不该在 429 用例里被触发）
type staticAuth struct{}

func (staticAuth) Headers() (map[string]string, error) { return map[string]string{}, nil }
func (staticAuth) Summary() (map[string]any, error)    { return map[string]any{}, nil }

// ---- 端到端：走完整 handleResponses 路径（聚合口/Codex 的真实入口）----

// 上游先回两次 429，第三次给正常 SSE → 客户端应拿到 200 且内容完整，
// 说明退避重试对「流式 Responses 路径」也是透明的。
func TestHandleResponsesAbsorbs429EndToEnd(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":14003,"msg":"too many requests","retry_policy":{"retry":true}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"delta\":{\"content\":\"收到\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := &Proxy{Name: "intl", Kind: "cred", Backend: up.URL, PrependSystem: true}
	p.auth = staticAuth{}
	p.client = up.Client()
	p.log = NewFileLogger("")

	body := `{"model":"deepseek-v4.1-flash","instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("429 未被吸收，客户端拿到 %d：%s", rec.Code, rec.Body.String()[:300])
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("应重试到第 3 次，实际 %d 次", got)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "收到") {
		t.Fatalf("响应内容缺失：%s", out[:300])
	}
	if strings.Contains(out, "429") || strings.Contains(out, "too many requests") {
		t.Fatalf("429 泄漏给了客户端：%s", out[:300])
	}
}

// 配额耗尽（6004）走完整路径 → 仍是 429，但带可操作提示，且只打一次上游
func TestHandleResponsesQuotaHintEndToEnd(t *testing.T) {
	var calls int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at 2030-01-01 10:00:00 UTC+8","displayMsg":{"zh":"配额超限","en":"limit"}}`)
	}))
	defer up.Close()

	p := &Proxy{Name: "intl", Kind: "cred", Backend: up.URL, PrependSystem: true}
	p.auth = staticAuth{}
	p.client = up.Client()
	p.log = NewFileLogger("")

	body := `{"model":"deepseek-v4.1-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":false}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleResponses(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("配额耗尽应返回 429，实际 %d", rec.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("配额耗尽不该重试，实际 %d 次", got)
	}
	if !strings.Contains(rec.Body.String(), "2030-01-01") {
		t.Fatalf("提示里应写清恢复时刻：%s", rec.Body.String())
	}
}
