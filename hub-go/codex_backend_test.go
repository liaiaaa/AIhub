package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// codexBackendProbe 记录假上游收到的请求，供分流断言。
type codexBackendProbe struct {
	officialHits int
	officialBody string
	lineHits     int
	lineBody     string
}

// setupCodexBackend 把官方透传地址与线路转发都指向本地假上游。
// 返回探针；不触碰真实网络、不消耗官方额度。
func setupCodexBackend(t *testing.T) *codexBackendProbe {
	t.Helper()
	probe := &codexBackendProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		probe.officialHits++
		probe.officialBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_probe","object":"response","output":[]}`))
	}))
	t.Cleanup(srv.Close)

	oldURL := officialBackendURLVar
	officialBackendURLVar = srv.URL
	t.Cleanup(func() { officialBackendURLVar = oldURL })

	// 官方 slug 集合固定，避免测试去跑本机 Codex CLI
	officialSlugMu.Lock()
	oldCache := officialSlugCache
	officialSlugCache = map[string]bool{"gpt-6.1-sol": true, "codex-auto-review": true}
	officialSlugMu.Unlock()
	t.Cleanup(func() {
		officialSlugMu.Lock()
		officialSlugCache = oldCache
		officialSlugMu.Unlock()
	})

	// 第三方线路：注入一个假适配器，直接回固定 JSON
	oldAdapters := lineAdapters
	lineAdapters = map[string]func(*Hub) lineAdapter{
		"dom": func(h *Hub) lineAdapter { return fakeCodexLine{h: h, probe: probe} },
	}
	t.Cleanup(func() { lineAdapters = oldAdapters })
	return probe
}

// fakeCodexLine 只实现 lineAdapter 里分流用得到的部分。
type fakeCodexLine struct {
	h     *Hub
	probe *codexBackendProbe
}

func (a fakeCodexLine) Key() string                       { return "dom" }
func (a fakeCodexLine) Running() bool                     { return true }
func (a fakeCodexLine) Start() (bool, string)             { return true, "ok" }
func (a fakeCodexLine) Stop() (bool, string)              { return true, "ok" }
func (a fakeCodexLine) NativeResponses() http.HandlerFunc { return nil }
func (a fakeCodexLine) Models() []string                  { return nil }
func (a fakeCodexLine) Chat(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	a.probe.lineHits++
	a.probe.lineBody = string(b)
	writeJSON(w, http.StatusOK, map[string]any{"object": "chat.completion", "choices": []any{}})
}

func postCodexBackend(t *testing.T, h *Hub, body string, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.codexBackendResponses(rec, req)
	return rec
}

func TestCodexBackendRoutesOfficialSlug(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"gpt-6.1-sol","stream":false}`, "Bearer probe-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，应为 200；body=%s", rec.Code, rec.Body.String())
	}
	if probe.officialHits != 1 {
		t.Errorf("官方上游命中 = %d，应为 1", probe.officialHits)
	}
	if probe.lineHits != 0 {
		t.Errorf("不该打到线路，实际 %d 次", probe.lineHits)
	}
	// store=false 由 officialFixBody 补上
	var m map[string]any
	if err := json.Unmarshal([]byte(probe.officialBody), &m); err != nil {
		t.Fatalf("上游 body 不是 JSON：%v", err)
	}
	if v, ok := m["store"].(bool); !ok || v {
		t.Errorf("store 应为 false，实际 %v", m["store"])
	}
}

func TestCodexBackendRoutesNonGptOfficialSlug(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"codex-auto-review"}`, "Bearer probe-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("非 gpt- 前缀的官方 slug 也要走官方，状态 = %d", rec.Code)
	}
	if probe.officialHits != 1 || probe.lineHits != 0 {
		t.Errorf("official=%d line=%d，应 official=1 line=0", probe.officialHits, probe.lineHits)
	}
}

func TestCodexBackendRoutesThirdParty(t *testing.T) {
	probe := setupCodexBackend(t)
	// responses 形态必须带 input：aggDispatch 内部要把它转成 chat 形态，
	// 缺 input 会先被既有转换逻辑拒掉（"input is required"），与分流无关。
	rec := postCodexBackend(t, &Hub{},
		`{"model":"dom.deepseek-v4-flash","stream":false,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("第三方应走线路，状态 = %d；body=%s", rec.Code, rec.Body.String())
	}
	if probe.lineHits != 1 {
		t.Errorf("线路命中 = %d，应为 1", probe.lineHits)
	}
	if probe.officialHits != 0 {
		t.Errorf("第三方不该打到官方，实际 %d 次", probe.officialHits)
	}
}

func TestCodexBackendUnknownModel404(t *testing.T) {
	probe := setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"model":"totally-unknown"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知模型状态 = %d，应为 404；body=%s", rec.Code, rec.Body.String())
	}
	if probe.officialHits != 0 || probe.lineHits != 0 {
		t.Errorf("未知模型不许打任何上游：official=%d line=%d", probe.officialHits, probe.lineHits)
	}
	if !strings.Contains(rec.Body.String(), "unknown_model") {
		t.Errorf("错误体应含 unknown_model，实际 %s", rec.Body.String())
	}
}

func TestCodexBackendMissingModel400(t *testing.T) {
	setupCodexBackend(t)
	rec := postCodexBackend(t, &Hub{}, `{"stream":true}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 model 状态 = %d，应为 400", rec.Code)
	}
}
