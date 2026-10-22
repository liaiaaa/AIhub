package main

// 2026-10-21 开源审查加固的回归测试。
//
// 加固前 main.go 的注释断言「内核只听 127.0.0.1 且无鉴权，跨域头不扩大攻击面」，
// 这是错的：CORS 不阻止请求发出，任意网页都能 POST /api/exit 停掉别人的服务，
// 或者用 <img src> 发 GET 触发状态变更。另外代码完全不校验 Host，DNS rebinding
// 连响应都能读走。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleGuard(t *testing.T) {
	var reached bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	h := withConsoleGuard(8801)(inner)

	do := func(method, path, host, origin string) (int, bool) {
		reached = false
		req := httptest.NewRequest(method, path, nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code, reached
	}

	// ── 正常使用必须照常通过 ──
	if code, ok := do("GET", "/api/status", "127.0.0.1:8801", ""); code != 200 || !ok {
		t.Errorf("正常 GET 被拦：code=%d reached=%v", code, ok)
	}
	if code, ok := do("POST", "/api/dom/start", "localhost:8801", ""); code != 200 || !ok {
		t.Errorf("正常 POST 被拦：code=%d reached=%v", code, ok)
	}
	// 同源页面发 POST 会带 Origin，必须放行并回显 CORS 头
	if code, ok := do("POST", "/api/dom/start", "127.0.0.1:8801", "http://127.0.0.1:8801"); code != 200 || !ok {
		t.Errorf("同源 POST 被拦：code=%d reached=%v", code, ok)
	}
	// IPv6 环回
	if code, ok := do("GET", "/api/status", "[::1]:8801", ""); code != 200 || !ok {
		t.Errorf("IPv6 环回被拦：code=%d reached=%v", code, ok)
	}

	// ── 攻击必须挡住 ──
	// DNS rebinding：Host 是攻击者域名
	if code, ok := do("GET", "/api/status", "evil.example.com", ""); code != 403 || ok {
		t.Errorf("恶意 Host 未被拦：code=%d reached=%v", code, ok)
	}
	// 跨站 Origin
	if code, ok := do("POST", "/api/exit", "127.0.0.1:8801", "https://evil.example.com"); code != 403 || ok {
		t.Errorf("跨站 Origin 未被拦：code=%d reached=%v", code, ok)
	}
	// <img src> 式 GET 触发状态变更
	if code, ok := do("GET", "/api/exit", "127.0.0.1:8801", ""); code != 405 || ok {
		t.Errorf("GET /api/exit 未被拦：code=%d reached=%v", code, ok)
	}
	if code, ok := do("GET", "/api/dom/stop", "127.0.0.1:8801", ""); code != 405 || ok {
		t.Errorf("GET /api/dom/stop 未被拦：code=%d reached=%v", code, ok)
	}
	if code, ok := do("GET", "/api/clients/takeover", "127.0.0.1:8801", ""); code != 405 || ok {
		t.Errorf("GET takeover 未被拦：code=%d reached=%v", code, ok)
	}
}

// CORS 不再发 `*`，只在同源时回显具体来源。
func TestConsoleGuardCORSNotWildcard(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := withConsoleGuard(8801)(inner)

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "127.0.0.1:8801"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("CORS 仍是通配 *，跨站页面能读走 /api/nick、/api/domain/creds 等响应")
	}

	req2 := httptest.NewRequest("GET", "/api/status", nil)
	req2.Host = "127.0.0.1:8801"
	req2.Header.Set("Origin", "http://127.0.0.1:8801")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if got := rr2.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:8801" {
		t.Errorf("同源 CORS 应回显具体来源，得到 %q", got)
	}
	if !strings.Contains(rr2.Header().Get("Vary"), "Origin") {
		t.Error("回显 Origin 时应带 Vary: Origin，否则缓存会串")
	}
}
