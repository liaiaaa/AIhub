package main

// 端到端：真 TLS 握手 + 真 CONNECT 隧道 + 真分流。
//
// 测试拓扑（全部在 127.0.0.1，不碰外网、不碰真凭据）：
//
//	测试客户端 ──CONNECT──▶ mitmProxy ──┬─ 模型在聚合目录 → 假聚合口（记录收到的 model）
//	                                     └─ 其他模型       → 假上游（记录字节）
//
// 「假上游」用 mitmCA 签的证书起 TLS，客户端信任同一张 CA —— 这样走的是
// 和真实场景完全一样的握手路径，只是把 api.anthropic.com 换成了本地域名。

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// mitmE2E 搭一套假环境：假聚合口 + 假上游 + 指向假上游的 mitmProxy。
type mitmE2E struct {
	proxy       *mitmProxy
	aggSrv      *httptest.Server
	upSrv       *httptest.Server
	proxyAddr   string
	aggHits     []string // 聚合口收到的 model 列表
	upHits      []string // 上游收到的 model 列表
	upRawBodies [][]byte
}

func newMITME2E(t *testing.T) *mitmE2E {
	t.Helper()
	e := &mitmE2E{}

	// 1) 假聚合口：把收到的 body 记下来，回一个可识别的 Anthropic 响应。
	e.aggSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		model, _ := m["model"].(string)
		e.aggHits = append(e.aggHits, model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_local","type":"message","role":"assistant",`+
			`"content":[{"type":"text","text":"from-agg:`+model+`"}],"model":"`+model+`"}`)
	}))
	t.Cleanup(e.aggSrv.Close)

	// 2) 假上游：TLS 服务端，证书由 mitmCA 签（域名 api.anthropic.com）。
	withTempDataRoot(t)
	ca, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("生成 CA 失败：%v", err)
	}
	e.upSrv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.upRawBodies = append(e.upRawBodies, body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		model, _ := m["model"].(string)
		e.upHits = append(e.upHits, model)
		// 把收到的关键头也回显，方便断言「原样透传」。
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"id":"msg_up","model":%q,"echo_anthropic_version":%q,"echo_beta":%q}`,
			model, r.Header.Get("anthropic-version"), r.Header.Get("anthropic-beta")))
	}))
	leaf, err := ca.leafFor("api.anthropic.com")
	if err != nil {
		t.Fatalf("签 leaf 失败：%v", err)
	}
	e.upSrv.TLS = &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
	}
	e.upSrv.StartTLS()
	t.Cleanup(e.upSrv.Close)

	// 3) mitmProxy：把「透传目标」改指到假上游。
	e.proxy = newMITMProxy(ca)
	upAddr := strings.TrimPrefix(e.upSrv.URL, "https://")
	e.proxy.upstreamOverride = upAddr // 测试专用：把 api.anthropic.com 的请求打到假上游
	// 假上游用的是 mitmCA 签的证书，验证链需要信任同一张 CA。
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	// ServerName 固定成 api.anthropic.com：假上游监听在 127.0.0.1:随机端口，
	// 而 leaf 只签了 api.anthropic.com 的 DNS SAN（真实场景也是这样）。
	e.proxy.upstreamClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				RootCAs:    roots,
				ServerName: "api.anthropic.com",
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	// 4) 聚合口地址覆盖：forwardLocal 里硬编码 aggPort，这里替换成假聚合口。
	aggAddr := strings.TrimPrefix(e.aggSrv.URL, "http://")
	e.proxy.aggOverride = aggAddr
	e.proxy.localClient = &http.Client{Timeout: 10 * time.Second}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("代理监听失败：%v", err)
	}
	e.proxyAddr = ln.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(e.proxy.serveHTTP)}
	e.proxy.mu.Lock()
	e.proxy.ln = ln
	e.proxy.srv = srv
	e.proxy.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		e.proxy.mu.Lock()
		e.proxy.ln = nil
		e.proxy.srv = nil
		e.proxy.mu.Unlock()
		_ = srv.Close()
	})

	return e
}

// doViaProxy 经代理发一次 HTTPS 请求（客户端信任测试 CA）。
func (e *mitmE2E) doViaProxy(t *testing.T, model string, headers map[string]string) (int, []byte) {
	t.Helper()
	ca, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("加载 CA 失败：%v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)

	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	proxyURL := "http://" + e.proxyAddr
	tr := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { return url.Parse(proxyURL) },
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("经代理请求失败：%v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestMITMRoutesThirdPartyToAgg 聚合目录里的模型必须改道到聚合口。
func TestMITMRoutesThirdPartyToAgg(t *testing.T) {
	e := newMITME2E(t)
	code, body := e.doViaProxy(t, "qoder.Qwen3.8-Flash", nil)
	if code != 200 {
		t.Fatalf("状态码 %d，body=%s", code, body)
	}
	if len(e.aggHits) != 1 {
		t.Fatalf("聚合口收到 %d 次请求，期望 1", len(e.aggHits))
	}
	if e.aggHits[0] != "qoder.Qwen3.8-Flash" {
		t.Errorf("聚合口收到的 model = %q", e.aggHits[0])
	}
	if len(e.upHits) != 0 {
		t.Errorf("上游不该被调用，实际收到 %v", e.upHits)
	}
	if !strings.Contains(string(body), "from-agg:qoder.Qwen3.8-Flash") {
		t.Errorf("响应不是来自聚合口：%s", body)
	}
}

// TestMITMPassesClaudeModelUpstream claude-* 且不在目录里的模型必须原样透传上游。
func TestMITMPassesClaudeModelUpstream(t *testing.T) {
	e := newMITME2E(t)
	code, body := e.doViaProxy(t, "claude-opus-4-5-20251101", map[string]string{
		"anthropic-beta": "prompt-caching-2024-07-31",
	})
	if code != 200 {
		t.Fatalf("状态码 %d，body=%s", code, body)
	}
	if len(e.upHits) != 1 {
		t.Fatalf("上游收到 %d 次请求，期望 1（agg 收到 %d）", len(e.upHits), len(e.aggHits))
	}
	if e.upHits[0] != "claude-opus-4-5-20251101" {
		t.Errorf("上游收到的 model = %q", e.upHits[0])
	}
	if len(e.aggHits) != 0 {
		t.Errorf("聚合口不该被调用，实际收到 %v", e.aggHits)
	}
	// 原样透传：头必须一字不改地到上游。
	var echo struct {
		AnthropicVersion string `json:"echo_anthropic_version"`
		Beta             string `json:"echo_beta"`
	}
	if err := json.Unmarshal(body, &echo); err != nil {
		t.Fatalf("响应不是 JSON：%s", body)
	}
	if echo.AnthropicVersion != "2023-06-01" {
		t.Errorf("anthropic-version 没透传，上游看到 %q", echo.AnthropicVersion)
	}
	if echo.Beta != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic-beta 没透传，上游看到 %q", echo.Beta)
	}
}

// TestMITMPassThroughIsByteIdentical 「原样透传」的核心承诺：body 一字节不改。
func TestMITMPassThroughIsByteIdentical(t *testing.T) {
	e := newMITME2E(t)
	// 带点容易在解析/重序列化里变形的写法：数字、null、嵌套、Unicode。
	original := `{"model":"claude-sonnet-4-6","max_tokens":1024,"temperature":0.7,` +
		`"system":null,"messages":[{"role":"user","content":[{"type":"text","text":"中文 🎯"}]}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(original))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	ca, _ := loadOrCreateMITMCA()
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	tr := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + e.proxyAddr)
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	resp.Body.Close()
	if len(e.upRawBodies) != 1 {
		t.Fatalf("上游收到 %d 个 body", len(e.upRawBodies))
	}
	if string(e.upRawBodies[0]) != original {
		t.Errorf("透传 body 被改动了\n收到: %s\n原始: %s", e.upRawBodies[0], original)
	}
}

// TestMITMUnknownModelGoesUpstream 目录里没有的模型名一律透传（不拦、不猜）。
func TestMITMUnknownModelGoesUpstream(t *testing.T) {
	e := newMITME2E(t)
	code, _ := e.doViaProxy(t, "some-unknown-model-xyz", nil)
	if code != 200 {
		t.Fatalf("状态码 %d", code)
	}
	if len(e.upHits) != 1 {
		t.Errorf("未知模型应透传上游，实际 up=%v agg=%v", e.upHits, e.aggHits)
	}
}

// TestMITMCounters 分流计数要能反映真实走向。
func TestMITMCounters(t *testing.T) {
	e := newMITME2E(t)
	e.doViaProxy(t, "qoder.Qwen3.8-Flash", nil)
	e.doViaProxy(t, "claude-opus-4-5-20251101", nil)
	if got := e.proxy.reqLocal.Load(); got != 1 {
		t.Errorf("routed_local = %d，期望 1", got)
	}
	if got := e.proxy.reqPass.Load(); got != 1 {
		t.Errorf("routed_passthrough = %d，期望 1", got)
	}
	if got := e.proxy.reqTotal.Load(); got != 2 {
		t.Errorf("requests = %d，期望 2", got)
	}
}

// TestMITMClaudeRoleNameStaysUpstream 3P 角色名（claude-sonnet-4-6 等）
// 在 1P 场景下必须透传 —— 这是「chat 走官方、code 走聚合」的关键分界。
func TestMITMClaudeRoleNameStaysUpstream(t *testing.T) {
	e := newMITME2E(t)
	for _, m := range []string{
		"claude-sonnet-4-6",
		"claude-opus-5",
		"claude-fable-5",
		"claude-haiku-4-5-20251001",
	} {
		e.doViaProxy(t, m, nil)
	}
	if len(e.aggHits) != 0 {
		t.Errorf("角色名不该进聚合口，实际 %v", e.aggHits)
	}
	if len(e.upHits) != 4 {
		t.Errorf("上游收到 %d 次，期望 4", len(e.upHits))
	}
}

// TestMITMStreamingFlush 流式响应必须逐块到达，不能等上游读完才一次性吐。
func TestMITMStreamingFlush(t *testing.T) {
	e := newMITME2E(t)
	// 换成会分两次写的 SSE 上游。
	e.upSrv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		if f != nil {
			f.Flush()
		}
		time.Sleep(120 * time.Millisecond)
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
		if f != nil {
			f.Flush()
		}
	})

	ca, _ := loadOrCreateMITMCA()
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	tr := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + e.proxyAddr)
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-5-20251101","stream":true}`))
	req.Header.Set("anthropic-version", "2023-06-01")
	t0 := time.Now()
	resp, err := (&http.Client{Transport: tr, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("读首行失败：%v", err)
	}
	firstAt := time.Since(t0)
	if !strings.Contains(line, "message_start") {
		t.Errorf("首行不是 message_start：%q", line)
	}
	// 上游中间睡了 120ms。首块如果 100ms 内就到，说明是真流式。
	if firstAt > 100*time.Millisecond {
		t.Errorf("首块耗时 %v，疑似攒完才发（应 < 100ms）", firstAt)
	}
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_stop") {
		t.Errorf("尾块缺失：%s", rest)
	}
}

// TestMITMNonTargetHostTunneled 非目标域名必须纯隧道，不做 TLS 终止。
func TestMITMNonTargetHostTunneled(t *testing.T) {
	e := newMITME2E(t)
	// 起一个自签 HTTPS 服务（证书与 mitmCA 无关）。
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnel-ok")
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")

	// 客户端信任那个服务自己的证书 —— 如果代理做了中间人，这里会验证失败。
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	tr := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + e.proxyAddr)
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("隧道请求失败：%v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "tunnel-ok" {
		t.Errorf("响应 = %q", b)
	}
	if e.proxy.reqTunnel.Load() != 1 {
		t.Errorf("tunneled = %d，期望 1", e.proxy.reqTunnel.Load())
	}
}
