package main

// Zcode 逆向接入（2026-09-27，对齐 TriDefender/zcode-api）新能力测试：
// 身份头两组形态、凭据确定性选取、端点路由改写与 fail-open、
// 实时目录解析与快照合并。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func zcodeTestIdentity() *zcodeIdentity {
	return &zcodeIdentity{
		appVersion:     "3.14.3",
		sourceTitle:    "cli",
		refererOrigin:  "https://zcode.z.ai",
		deviceMid:      "1bf6b9a2-unit-test",
		platform:       "win32",
		arch:           "x64",
		osVersion:      "10.0.26200",
		releaseChannel: "production",
		clientLanguage: "zh-CN",
		clientTimezone: "Asia/Shanghai",
	}
}

// g6n 组（LLM 补全）：带 X-ZCode-Agent，绝不带 X-Device-Mid。
func TestZcodeLLMIdentityHeaders(t *testing.T) {
	h := zcodeTestIdentity().llmHeaders()
	if h["User-Agent"] != "ZCode/3.14.3" {
		t.Errorf("User-Agent = %q, want ZCode/3.14.3", h["User-Agent"])
	}
	if h["X-ZCode-Agent"] != "glm" {
		t.Errorf("X-ZCode-Agent = %q, want glm", h["X-ZCode-Agent"])
	}
	if h["X-Title"] != "Z Code@cli" {
		t.Errorf("X-Title = %q", h["X-Title"])
	}
	if h["X-Platform"] != "win32-x64" || h["X-Os-Category"] != "windows" || h["X-Os-Version"] != "10.0.26200" {
		t.Errorf("平台头不符: %v", h)
	}
	if _, has := h["X-Device-Mid"]; has {
		t.Error("LLM 补全面不应携带 X-Device-Mid")
	}
}

// TV 组（控制面）：带 X-Device-Mid，无 X-ZCode-Agent。
func TestZcodeContextIdentityHeaders(t *testing.T) {
	h := zcodeTestIdentity().contextHeaders()
	if h["X-Device-Mid"] != "1bf6b9a2-unit-test" {
		t.Errorf("X-Device-Mid = %q", h["X-Device-Mid"])
	}
	if _, has := h["X-ZCode-Agent"]; has {
		t.Error("控制面不应携带 X-ZCode-Agent")
	}
	if h["User-Agent"] != "ZCode/3.14.3" {
		t.Errorf("User-Agent = %q", h["User-Agent"])
	}
}

// 可打印 ASCII 门：非法值整体丢弃，由调用方省略该 header。
func TestZcodePrintableGate(t *testing.T) {
	if got := zcodePrintable("  ok-123 "); got != "ok-123" {
		t.Errorf("zcodePrintable = %q", got)
	}
	if zcodePrintable("bad\nvalue") != "" || zcodePrintable("") != "" {
		t.Error("非法/空值应返回空串")
	}
}

// 凭据确定性选取：两族 key 并存时优先 individual-coding-plan，并带出 JWT。
func TestZcodeCredentialPrefersIndividualPlan(t *testing.T) {
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "unit-test-secret")
	key := zcodeCipherKey()
	creds := map[string]string{
		"account-provider:coding-plan:account:zai-start-plan:account:uuid:api-key":             zcodeTestEncrypt(t, key, "start-plan-key.secret"),
		"account-provider:coding-plan:account:zai-individual-coding-plan:account:uuid:api-key": zcodeTestEncrypt(t, key, "individual-key.secret"),
		"zcodejwttoken": zcodeTestEncrypt(t, key, "test-jwt-payload"),
	}
	raw, _ := json.Marshal(creds)
	path := t.TempDir() + "/credentials.json"
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	t.Setenv("ZCODE_CREDENTIALS_PATH", path)

	a, err := NewZcodeAuth()
	if err != nil {
		t.Fatalf("NewZcodeAuth: %v", err)
	}
	if got := a.Key(); got != "individual-key.secret" {
		t.Errorf("Key() = %q, want individual-coding-plan key（确定性选取）", got)
	}
	if got := a.JWT(); got != "test-jwt-payload" {
		t.Errorf("JWT() = %q, want test-jwt-payload", got)
	}
}

// 路由键归一化：缺省端口、去尾斜杠、大小写。
func TestZcodeRoutingKey(t *testing.T) {
	cases := map[string]string{
		"https://api.z.ai/api/coding/paas/v4":      "https//api.z.ai:443/api/coding/paas/v4",
		"https://api.z.ai:443/api/coding/paas/v4/": "https//api.z.ai:443/api/coding/paas/v4",
		"https://API.Z.AI/api/coding/paas/v4":      "https//api.z.ai:443/api/coding/paas/v4",
		"http://api.z.ai/api/coding/paas/v4":       "http//api.z.ai:443/api/coding/paas/v4",
		"https://zcode.z.ai/api/v1/ultra-zai":      "https//zcode.z.ai:443/api/v1/ultra-zai",
	}
	for in, want := range cases {
		if got := zcodeRoutingKey(in); got != want {
			t.Errorf("zcodeRoutingKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// 改写命中 + 查询串保留。
func TestZcodeRoutingRewrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"proxyEndpoint":{"mapping":[
			{"from":"https://api.z.ai/api/coding/paas/v4","to":"https://zcode.z.ai/api/v1/ultra-zai"}]}}}`)
	}))
	defer srv.Close()

	id := zcodeTestIdentity()
	r := newZcodeRouting(id, func() string { return "test-key" })
	r.origin = srv.URL
	r.client = srv.Client()

	got := r.resolve("https://api.z.ai/api/coding/paas/v4/chat/completions?x=1")
	want := "https://zcode.z.ai/api/v1/ultra-zai/chat/completions?x=1"
	if got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}
	if !r.HasSnapshot() || r.SnapshotSize() != 1 {
		t.Errorf("快照状态不符: has=%v n=%d", r.HasSnapshot(), r.SnapshotSize())
	}
}

// fail-open：上游 500 / 业务码非 0 / 表项非法 → 原 URL 直发。
func TestZcodeRoutingFailOpen(t *testing.T) {
	var handler http.HandlerFunc
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r)
	}))
	defer srv.Close()
	handler = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":3001,"msg":"parameter error"}`)
	}

	id := zcodeTestIdentity()
	r := newZcodeRouting(id, nil)
	r.origin = srv.URL
	r.client = srv.Client()

	orig := "https://api.z.ai/api/coding/paas/v4/chat/completions"
	if got := r.resolve(orig); got != orig {
		t.Errorf("业务码非 0 应原样返回, got %q", got)
	}
	if r.HasSnapshot() {
		t.Error("失败不应落快照")
	}

	// 服务端 500 同样 fail-open
	r2 := newZcodeRouting(id, nil)
	r2.origin = srv.URL
	r2.client = srv.Client()
	handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	if got := r2.resolve(orig); got != orig {
		t.Errorf("HTTP 500 应原样返回, got %q", got)
	}
}

// client/configs 条目 → 目录条目，再与内置快照合并。
func TestZcodeRemoteCatalogMerge(t *testing.T) {
	fixture := `{"code":0,"data":{"builtinModels":[
		{"modelId":"GLM-5.3","name":"GLM-5.3","contextWindow":1000000,"priority":99,
		 "capabilities":{},"reasoning":{"levels":{"low":{},"high":{},"max":{}},"defaultLevel":"max"},
		 "modalities":{},"maxCompletionTokens":128000},
		{"modelId":"GLM-5.3-Flash","name":"GLM-5.3-Flash","contextWindow":1000000,"priority":100,
		 "capabilities":{"vision":true},"reasoning":{"levels":{"low":{},"high":{},"max":{}},"defaultLevel":"max"},
		 "modalities":{"input":["text","image","video"]},"maxCompletionTokens":128000}],
		"providers":[{"id":"z-ai","schema":"openai:chat","baseUrl":"https://api.z.ai/api/coding/paas/v4",
		 "models":[{"modelId":"GLM-5.2","name":"GLM-5.2","contextWindow":1048576,"priority":100,
		  "capabilities":{},"reasoning":{},"maxCompletionTokens":131072}]}]}}`

	var parsed zcodeClientConfigs
	if err := json.Unmarshal([]byte(fixture), &parsed); err != nil {
		t.Fatalf("fixture 解析: %v", err)
	}
	var remote []zcodeModelEntry
	for _, m := range parsed.Data.BuiltinModels {
		if e, ok := zcodeRemoteToEntry(m); ok {
			remote = append(remote, e)
		}
	}
	for _, prov := range parsed.Data.Providers {
		for _, m := range prov.Models {
			if e, ok := zcodeRemoteToEntry(m); ok {
				remote = append(remote, e)
			}
		}
	}
	// 远端独有模型（验证快照独有条目的保留路径）
	remote = append(remote, zcodeModelEntry{Key: "glm-5.4-test", DisplayName: "GLM-5.4-Test", Vendor: "zai",
		MaxInputTokens: 200000, ContextWindows: []string{"200K"}})
	if len(remote) != 4 {
		t.Fatalf("远端条目 = %d, want 4", len(remote))
	}
	if remote[0].Key != "glm-5.3" || remote[0].DefaultEffort != "max" {
		t.Errorf("GLM-5.3 映射不符: %+v", remote[0])
	}
	if strings.Join(remote[0].Efforts, ",") != "low,high,max" {
		t.Errorf("档位顺序 = %v, want low,high,max", remote[0].Efforts)
	}
	if !remote[1].IsVision {
		t.Error("GLM-5.3-Flash 应识别为视觉模型")
	}

	snapshot := zcodeDefaultCatalog()
	merged := mergeZcodeCatalog(remote, snapshot)
	// 快照 11 条中 glm-5.3/5.3-flash/5.2 与远端重合（远端规格为准），其余保留；
	// 远端独有 glm-5.4-test 追加 → 12 条
	if len(merged) != len(snapshot)+1 {
		t.Errorf("合并后 = %d 条, want %d", len(merged), len(snapshot)+1)
	}
	if merged[0].Key != "glm-5.3" || !merged[0].IsDefault {
		t.Errorf("默认模型应排首位: %+v", merged[0])
	}
	byKey := map[string]zcodeModelEntry{}
	for _, e := range merged {
		byKey[e.Key] = e
	}
	if e := byKey["glm-5.3"]; e.SupportsDisable {
		t.Error("5.3 系快照结论「不可关闭」应被保留")
	}
	if e := byKey["glm-5.2"]; !e.IsReasoning || !e.SupportsDisable {
		t.Error("glm-5.2 快照元数据（reasoning/可关闭）应被叠加")
	}
	if e := byKey["glm-5.2"]; e.MaxInputTokens != 1048576 || len(e.ContextWindows) != 1 || e.ContextWindows[0] != "1M" {
		t.Errorf("glm-5.2 远端规格应生效: %+v", e)
	}
	if _, ok := byKey["glm-4.6"]; !ok {
		t.Error("快照独有模型 glm-4.6 应保留")
	}
}

// 人类可读上下文标签：整除优先（1M/200K），十进制兜底（128000→128K）。
func TestZcodeHumanizeTokens(t *testing.T) {
	cases := map[int]string{
		1000000: "1M", 1048576: "1M", 204800: "200K",
		131072: "128K", 128000: "128K", 4096: "4K",
	}
	for in, want := range cases {
		if got := zcodeHumanizeTokens(in); got != want {
			t.Errorf("zcodeHumanizeTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

// 上游 LLM 请求携带桌面端身份头（g6n），且默认无设备号。
func TestZcodeUpstreamIdentityHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewZcodeProxy()
	p.identity = zcodeTestIdentity()
	p.Endpoint = srv.URL
	p.apiKey = "zk-test-key"
	p.client = srv.Client()

	resp, err := p.zcodeUpstream(context.Background(), []byte("{}"))
	if err != nil {
		t.Fatalf("zcodeUpstream: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if ua := got.Get("User-Agent"); !strings.HasPrefix(ua, "ZCode/") {
		t.Errorf("User-Agent = %q, want ZCode/前缀", ua)
	}
	// 服务端读回的 header 名已被 textproto 规范化（Http-Referer），
	// 值正确即证明发出；写入侧保留官方大小写由 setIdentityHeaders 锁定。
	if refs := got["Http-Referer"]; len(refs) != 1 || refs[0] != "https://zcode.z.ai" {
		t.Errorf("Http-Referer = %v, want https://zcode.z.ai", refs)
	}
	req, _ := http.NewRequest("POST", "https://api.z.ai/api/coding/paas/v4/chat/completions", nil)
	setIdentityHeaders(req, zcodeTestIdentity().llmHeaders())
	if _, ok := req.Header["HTTP-Referer"]; !ok {
		t.Error("写入侧应保留官方原样大小写 HTTP-Referer（不经 Header.Set 规范化）")
	}
	if got.Get("X-ZCode-Agent") != "glm" {
		t.Errorf("X-ZCode-Agent = %q", got.Get("X-ZCode-Agent"))
	}
	if got.Get("X-Device-Mid") != "" {
		t.Error("LLM 请求不应携带 X-Device-Mid")
	}
}

// 目录白名单：只透出套餐额度覆盖的两个模型，内部目录保留全量。
func TestZcodeModelAllowlist(t *testing.T) {
	listed := zcodeFilterCatalog(zcodeDefaultCatalog())
	if len(listed) != 2 {
		t.Fatalf("白名单过滤后 = %d 条, want 2", len(listed))
	}
	if listed[0].Key != "glm-5.3" || listed[1].Key != "glm-5.3-flash" {
		t.Errorf("白名单条目不符: %s / %s", listed[0].Key, listed[1].Key)
	}
	// 内部目录不受影响（thinking 元数据对透传模型照常可用）
	if e := zcodeEntryFor(zcodeDefaultCatalog(), "glm-5.2"); e == nil {
		t.Error("内部全量目录不应被白名单改动")
	}
	// 未知 key 过滤为空
	if got := zcodeFilterCatalog([]zcodeModelEntry{{Key: "glm-9.9"}}); len(got) != 0 {
		t.Errorf("白名单外应被滤除: %v", got)
	}
}

// 3007 自救：中继返回 400/502 且错误体带 3007 时，自动换 token 重试；
// 非加密失败原样透出，连续 3007 两轮后放弃。
func TestZcodeRelayCaptchaSalvage(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1: // 中继把上游 400+3007 包成 502 转发
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"type":"translation_failed","message":"upstream returned 400: {\"code\":3007,\"msg\":\"captcha verify failed\"}"}}`))
		default: // 自救后成功
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
		}
	}))
	defer srv.Close()

	p := NewZcodeProxy()
	p.Endpoint = "https://api.z.ai/api/coding/paas/v4"
	p.identity = zcodeTestIdentity()
	p.apiKey = "direct-key"
	p.relayURL = srv.URL
	p.relayKey = "relay-key"
	p.client = srv.Client()

	resp, err := p.zcodeUpstream(context.Background(), []byte("{}"))
	if err != nil {
		t.Fatalf("自救重试后应成功: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("自救后 status = %d, want 200", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("上游调用次数 = %d, want 2（一次失败 + 一次自救）", calls)
	}

	// 连续 3007：两轮后放弃，错误体原样透出
	calls = 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3007,"msg":"captcha verify failed"}`))
	}))
	defer srv2.Close()
	p2 := NewZcodeProxy()
	p2.identity = zcodeTestIdentity()
	p2.apiKey = "direct-key"
	p2.relayURL = srv2.URL
	p2.relayKey = "relay-key"
	p2.client = srv2.Client()

	resp2, err := p2.zcodeUpstream(context.Background(), []byte("{}"))
	if err != nil {
		t.Fatalf("3007 耗尽不应报网络错误: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 400 || !strings.Contains(string(body), "3007") {
		t.Errorf("耗尽后应原样透出 400+3007, got %d %s", resp2.StatusCode, truncate(string(body), 120))
	}
	if calls != 3 {
		t.Errorf("连续 3007 应试满 3 次（1 失败 + 2 自救）, got %d", calls)
	}
}

// 验证码熔断器：连续 3 轮 3007 自救耗尽 → 熔断打开（秒回 503，不再打上游）；
// 成功一次即复位。
func TestZcodeCaptchaBreaker(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3007,"msg":"captcha verify failed"}`))
	}))
	defer srv.Close()

	p := NewZcodeProxy()
	p.identity = zcodeTestIdentity()
	p.apiKey = "direct-key"
	p.relayURL = srv.URL
	p.relayKey = "relay-key"
	p.client = srv.Client()

	// 3 轮完整失败（每轮 3 次上游调用：1 失败 + 2 自救）
	for i := 0; i < 3; i++ {
		resp, err := p.zcodeUpstream(context.Background(), []byte("{}"))
		if err != nil {
			t.Fatalf("第 %d 轮不应报错（应透出 400）: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if _, open := p.captchaBreakerOpen(); !open {
		t.Fatal("3 轮失败后熔断应打开")
	}
	callsBefore := calls

	// 熔断期：秒回 captchaBreakerError，不再打上游
	resp, err := p.zcodeUpstream(context.Background(), []byte("{}"))
	var breaker *captchaBreakerError
	if !errors.As(err, &breaker) {
		t.Fatalf("熔断期应返回 captchaBreakerError, got %v", err)
	}
	if resp != nil {
		t.Error("熔断期不应有上游响应")
	}
	if calls != callsBefore {
		t.Errorf("熔断期不应打上游: %d → %d", callsBefore, calls)
	}

	// 成功一次即复位
	p.noteCaptchaSuccess()
	if _, open := p.captchaBreakerOpen(); open {
		t.Error("成功后熔断应复位")
	}
}

// 中继模式：请求打向本地中继、密钥切换为中继本地钥，直连端点不参与。
func TestZcodeRelayMode(t *testing.T) {
	var gotAuth, gotUA string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer relay.Close()

	p := NewZcodeProxy()
	p.Endpoint = "https://api.z.ai/api/coding/paas/v4"
	p.identity = zcodeTestIdentity()
	p.apiKey = "direct-key"
	p.relayURL = relay.URL
	p.relayKey = "relay-key"
	p.client = relay.Client()

	resp, err := p.zcodeUpstream(context.Background(), []byte("{}"))
	if err != nil {
		t.Fatalf("zcodeUpstream: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if gotAuth != "Bearer relay-key" {
		t.Errorf("中继模式 Authorization = %q, want Bearer relay-key", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "ZCode/") {
		t.Errorf("中继请求仍应携带桌面端身份头, UA = %q", gotUA)
	}
}

// ZCODE_RELAY_URL=off 强制关闭中继；显式地址+密钥直接生效。
func TestZcodeRelayDetectEnv(t *testing.T) {
	p := NewZcodeProxy()
	t.Setenv("ZCODE_RELAY_URL", "off")
	p.detectRelay(true)
	if url, _, ok := p.relayTarget(); ok || url != "" {
		t.Errorf("off 应强制直连, got %q", url)
	}

	p2 := NewZcodeProxy()
	t.Setenv("ZCODE_RELAY_URL", "http://127.0.0.1:9999")
	t.Setenv("ZCODE_RELAY_KEY", "custom-key")
	p2.detectRelay(true)
	url, key, ok := p2.relayTarget()
	if !ok || url != "http://127.0.0.1:9999" || key != "custom-key" {
		t.Errorf("显式中继地址应直接生效: ok=%v url=%q key=%q", ok, url, key)
	}
}

// 请求启动节流：密集启动被拉平到最小间隔，且不串行长流。
func TestZcodeRelayStartPacing(t *testing.T) {
	t.Setenv("ZCODE_RELAY_START_INTERVAL_MS", "60")
	// 重置 once 缓存（测试进程内只此一处依赖实现细节）
	zcodeRelayIntervalOnce = sync.Once{}

	p := NewZcodeProxy()
	start := time.Now()
	for i := 0; i < 3; i++ {
		p.relayWaitStart(context.Background())
	}
	elapsed := time.Since(start)
	// 3 次启动：第 1 次立即可走，2、3 次各等一个间隔
	if elapsed < 110*time.Millisecond {
		t.Errorf("节流未生效: elapsed=%v, want >=110ms", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("节流过度: elapsed=%v", elapsed)
	}

	// 关闭节流：立即返回
	t.Setenv("ZCODE_RELAY_START_INTERVAL_MS", "0")
	zcodeRelayIntervalOnce = sync.Once{}
	p2 := NewZcodeProxy()
	begin := time.Now()
	for i := 0; i < 3; i++ {
		p2.relayWaitStart(context.Background())
	}
	if d := time.Since(begin); d > 10*time.Millisecond {
		t.Errorf("interval=0 应立即返回: %v", d)
	}
}
