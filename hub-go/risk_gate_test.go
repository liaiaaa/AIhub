package main

// 冷却闸门（2026-10-25）：台账里已有 11140 / 11128 / 6004 记录时，请求路径
// 必须在打上游之前拦下来。这组测试同时钉住「闸门真的不发上游」与 fail-open。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gateFakeAuth 只实现 AuthProvider 的两个方法，Summary 直接给 uid。
type gateFakeAuth struct {
	uid  string
	nick string
	err  error
}

func (f gateFakeAuth) Headers() (map[string]string, error) { return map[string]string{}, nil }
func (f gateFakeAuth) Summary() (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"uid": f.uid, "nickname": f.nick}, nil
}

func resetGateState(t *testing.T) {
	t.Helper()
	resetRateLimit(t)
	channelBlockHitsReset()
}

// 账号级风控（11140）命中后：任何模型都该被本地拦下，且返回 Retry-After。
func TestGateCoolingBlocksAccountRisk(t *testing.T) {
	resetGateState(t)
	rateLimitRecord("uid-gate", "号", rlKindAccount, "", 11140, "账号级风控(11140)", time.Time{})

	p := &Proxy{}
	w := httptest.NewRecorder()
	if !p.gateCooling(w, gateFakeAuth{uid: "uid-gate"}, "glm-5.3-flash", "rid", nil) {
		t.Fatal("账号级风控应被闸门拦下")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("应回 429，实际 %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 必须带 Retry-After，否则客户端不知道等多久")
	}
	body := w.Body.String()
	for _, want := range []string{"账号级风控", "本地闸门", "hub_hint"} {
		if !strings.Contains(body, want) {
			t.Errorf("响应缺 %q：%s", want, body)
		}
	}
}

// 模型级 6004 只拦被限的那个模型，同账号换模型应放行。
func TestGateCoolingIsModelScoped(t *testing.T) {
	resetGateState(t)
	rateLimitRecord("uid-m", "号", rlKindModel, "deepseek-v4.1-flash", 6004, "用量限流(6004)", time.Now().Add(30*time.Minute))
	auth := gateFakeAuth{uid: "uid-m"}

	p := &Proxy{}
	if !p.gateCooling(httptest.NewRecorder(), auth, "deepseek-v4.1-flash", "rid", nil) {
		t.Error("被限的模型应拦下")
	}
	if p.gateCooling(httptest.NewRecorder(), auth, "hy4-preview", "rid", nil) {
		t.Error("同账号其它模型不该被拦（模型级限流）")
	}
}

// 渠道拦截闸门：窗口内累计 3 次才开，开出来后任意模型都拦。
func TestGateCoolingChannelGate(t *testing.T) {
	resetGateState(t)
	auth := gateFakeAuth{uid: "uid-c"}
	p := &Proxy{}

	for i := 1; i <= channelBlockGateHits; i++ {
		_, gated := rateLimitRecordChannel("uid-c", "号")
		if i < channelBlockGateHits && gated {
			t.Fatalf("第 %d 次就开闸门了，单次误拦不该停摆", i)
		}
		if i == channelBlockGateHits && !gated {
			t.Fatal("第 3 次应开闸门")
		}
	}
	if !p.gateCooling(httptest.NewRecorder(), auth, "任意模型", "rid", nil) {
		t.Error("渠道闸门开出后应拦下请求")
	}
	// 台账里要有可读原因，看板才能显示
	found := false
	for _, e := range rateLimitSnapshot() {
		if e.Kind == rlKindChannel && strings.Contains(e.Reason, "11128") {
			found = true
		}
	}
	if !found {
		t.Error("渠道闸门应写进台账，且原因里带 11128")
	}
}

// fail-open：认证拿不到 uid / Summary 报错时必须放行，闸门不能把线路锁死。
func TestGateCoolingFailsOpen(t *testing.T) {
	resetGateState(t)
	p := &Proxy{}
	if p.gateCooling(httptest.NewRecorder(), nil, "m", "rid", nil) {
		t.Error("auth 为 nil 应放行")
	}
	if p.gateCooling(httptest.NewRecorder(), gateFakeAuth{err: io.ErrUnexpectedEOF}, "m", "rid", nil) {
		t.Error("Summary 报错应放行")
	}
	if p.gateCooling(httptest.NewRecorder(), gateFakeAuth{uid: ""}, "m", "rid", nil) {
		t.Error("uid 为空应放行")
	}
}

// 台账干净时不该拦任何请求（别把正常路径做成随机 429）。
func TestGateCoolingNoRecordPasses(t *testing.T) {
	resetGateState(t)
	p := &Proxy{}
	if p.gateCooling(httptest.NewRecorder(), gateFakeAuth{uid: "uid-clean"}, "glm-5.3-flash", "rid", nil) {
		t.Error("没有台账记录时不该拦")
	}
}

// 端到端：handleChat 在闸门命中时**一次都不打上游**。
func TestHandleChatGateStopsBeforeUpstream(t *testing.T) {
	resetGateState(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}))
	defer srv.Close()

	rateLimitRecord("uid-chat", "号", rlKindAccount, "", 11140, "账号级风控(11140)", time.Time{})

	p := &Proxy{Backend: srv.URL, DefaultModel: "glm-5.3-flash"}
	p.auth = gateFakeAuth{uid: "uid-chat"}
	p.client = srv.Client()

	body := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.handleChat(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("应回 429，实际 %d：%s", w.Code, w.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("闸门命中时不该打上游，实际打了 %d 次", got)
	}
}
