package main

// OpenCode 免费档线路的回归测试（2026-10-15）。
//
// 全部离线：只验证「上游免费档闸门」要求的请求形状与目录元数据，
// 不打真实网络（真网络验证在部署后单独跑）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 官方 Identifier.descending() 布局：ses_ + 12 位降序时间戳 hex + 14 位 Base62。
var ocSessRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// 低 12 位必须是 0xFC0-0xFFF（counter 1-63），即第 10 位起为 f + [c-f] + hex。
var ocTimePartRe = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

func TestOpencodeSessionIDShape(t *testing.T) {
	for _, sig := range []string{"a", "big-pickle\x00hello", "cat-jam-free\x00" + strings.Repeat("x", 5000)} {
		sid := opencodeSessionID(sig)
		if !ocSessRe.MatchString(sid) {
			t.Fatalf("会话 id 布局不合官方格式：%q", sid)
		}
		if !ocTimePartRe.MatchString(sid[4:16]) {
			t.Fatalf("时间部分不在 counter 1-63 窗口（上游会 403）：%q", sid)
		}
	}
}

// 同一信号必须稳定同值（上游按会话做 prompt cache 亲和）。
func TestOpencodeSessionIDStable(t *testing.T) {
	a := opencodeSessionID("model\x00body")
	b := opencodeSessionID("model\x00body")
	if a != b {
		t.Fatalf("同信号不稳定：%q vs %q", a, b)
	}
	if opencodeSessionID("other") == a {
		t.Fatal("不同信号撞了同一个会话 id")
	}
}

func TestOpencodeProjectIDIsBareHex40(t *testing.T) {
	p := opencodeProjectID("aihub-opencode")
	if len(p) != 40 {
		t.Fatalf("项目 id 必须 40 字符，实际 %d：%q", len(p), p)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p) {
		t.Fatalf("必须是裸小写 hex（prj_* 会被免费档拒）：%q", p)
	}
}

// 免费档硬要求：stream:true + 5 个官方核心工具，缺一个就 403。
func TestOpencodeBuildBodyGate(t *testing.T) {
	raw := map[string]any{
		"model":    "big-pickle",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false, // 客户端要非流式，也必须改成 true
	}
	body := opencodeBuildBody(raw)
	if body["stream"] != true {
		t.Fatalf("上游只收流式，stream 必须为 true，实际 %v", body["stream"])
	}
	tools, ok := body["tools"].([]any)
	if !ok {
		t.Fatal("tools 缺失")
	}
	names := map[string]bool{}
	for _, it := range tools {
		if n := opencodeToolName(it.(map[string]any)); n != "" {
			names[n] = true
		}
	}
	for _, want := range opencodeCoreTools {
		if !names[want] {
			t.Fatalf("缺核心工具 %q（上游会 403 FreeTierError）", want)
		}
	}
}

// 核心工具必须带官方真实定义（合成 stub 会被逐字节比对拒）。
func TestOpencodeCoreToolsUseGenuineDefs(t *testing.T) {
	for _, n := range opencodeCoreTools {
		tool := opencodeCoreToolWire(n)
		fn, _ := tool["function"].(map[string]any)
		desc, _ := fn["description"].(string)
		if desc == "" || strings.HasPrefix(desc, "Agent tool ") {
			t.Fatalf("工具 %q 用的是占位描述，上游会 403：%q", n, desc)
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if len(props) == 0 {
			t.Fatalf("工具 %q 参数 schema 为空，上游会 403", n)
		}
	}
}

// 客户端自己的工具要保留；与官方同名的要换成官方定义。
func TestOpencodeBuildBodyKeepsClientTools(t *testing.T) {
	raw := map[string]any{
		"model":    "big-pickle",
		"messages": []any{},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "get_weather", "description": "查天气",
				"parameters": map[string]any{"type": "object"}}},
			// 客户端自带一个简化版 read，必须被官方定义替换
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": "我的简化读文件",
				"parameters": map[string]any{"type": "object"}}},
		},
	}
	body := opencodeBuildBody(raw)
	tools, _ := body["tools"].([]any)
	var weather, read any
	for _, it := range tools {
		m := it.(map[string]any)
		switch opencodeToolName(m) {
		case "get_weather":
			weather = m
		case "read":
			read = m
		}
	}
	if weather == nil {
		t.Fatal("客户端自定义工具被丢掉了")
	}
	got := read.(map[string]any)["function"].(map[string]any)["description"].(string)
	if got == "我的简化读文件" {
		t.Fatal("同名工具没换成官方定义（上游逐字节比对会 403）")
	}
	if want := opencodeTools()["read"].Description; got != want {
		t.Fatalf("read 描述不是官方值")
	}
}

// 目录必须带「免费」标注与上下文（用户硬性要求）。
func TestOpencodeModelsCatalogHasContext(t *testing.T) {
	p := NewOpencodeProxy()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	p.handleModels(w, req)

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("目录不是合法 JSON：%s", w.Body.String()[:200])
	}
	if len(resp.Data) != len(opencodeFreeModels) {
		t.Fatalf("目录条目 %d，应为 %d", len(resp.Data), len(opencodeFreeModels))
	}
	for _, e := range resp.Data {
		name, _ := e["name"].(string)
		if !strings.Contains(name, "免费") {
			t.Errorf("%v 的显示名缺「免费」：%q", e["id"], name)
		}
		if !strings.Contains(name, "上下文") {
			t.Errorf("%v 的显示名缺上下文标注：%q", e["id"], name)
		}
		if e["contextWindow"] == nil {
			t.Errorf("%v 缺 contextWindow 字段", e["id"])
		}
		if e["max_output_tokens"] == nil {
			t.Errorf("%v 缺 max_output_tokens 字段", e["id"])
		}
	}
}

// 只挂免费模型：非免费 id 必须被明确拒绝，不能偷偷转发。
func TestOpencodeRejectsNonFreeModel(t *testing.T) {
	p := NewOpencodeProxy()
	body, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-5-5", // 付费模型
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.handleChat(w, req)

	if w.Code != 400 {
		t.Fatalf("付费模型应被拒（400），实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "只挂免费模型") {
		t.Fatalf("错误提示没说清原因：%s", w.Body.String()[:200])
	}
}

// 每个模型都要有上下文数据（用户要求「一定要标记清楚上下文为多少」）。
func TestOpencodeAllModelsHaveLimits(t *testing.T) {
	for _, m := range opencodeFreeModels {
		if m.Context <= 0 {
			t.Errorf("%s 上下文缺失", m.ID)
		}
		if m.Output <= 0 {
			t.Errorf("%s 输出上限缺失", m.ID)
		}
		if m.Name == "" {
			t.Errorf("%s 显示名缺失", m.ID)
		}
	}
}

func TestOpencodeCtxLabel(t *testing.T) {
	cases := []struct {
		ctx, out int
		want     string
	}{
		{1048576, 131072, "上下文 1M/输出 128K"},
		{1000000, 65536, "上下文 1M/输出 64K"},
		{262144, 262144, "上下文 256K/输出 256K"},
		{200000, 32000, "上下文 200K/输出 32K"},
		{0, 0, "上下文未知"},
	}
	for _, c := range cases {
		if got := opencodeCtxLabel(c.ctx, c.out); got != c.want {
			t.Errorf("opencodeCtxLabel(%d,%d) = %q, want %q", c.ctx, c.out, got, c.want)
		}
	}
}

// withOpencodeModelFixtures 临时换掉线路的模型表，让「协议守卫」这类逻辑测试
// 不依赖当前挂了哪几个模型 —— 名单会随上游下架变动（2026-10-18 就从 9 个收到 2 个），
// 测试要的是守卫行为本身，不是某一次名单快照。
func withOpencodeModelFixtures(t *testing.T, ms ...opencodeModel) {
	t.Helper()
	old := opencodeModelIDs
	m := make(map[string]opencodeModel, len(ms))
	for _, x := range ms {
		m[x.ID] = x
	}
	opencodeModelIDs = m
	t.Cleanup(func() { opencodeModelIDs = old })
}

// 协议支持面是按模型实测出来的，两边都要有守卫，且提示要指向另一条路径。
func TestOpencodeProtocolGuardInChat(t *testing.T) {
	withOpencodeModelFixtures(t, opencodeModel{ID: "resp-only", Context: 1000000, NoChat: true})
	p := NewOpencodeProxy()
	body, _ := json.Marshal(map[string]any{
		"model":    "resp-only", // 只收 responses
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.handleChat(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "/v1/responses") {
		t.Fatalf("chat 侧对只收 responses 的模型没有给出可操作提示：%d %s", w.Code, w.Body.String()[:200])
	}
}

func TestOpencodeProtocolGuardInResponses(t *testing.T) {
	withOpencodeModelFixtures(t, opencodeModel{ID: "chat-only", Context: 1000000, NoResp: true})
	p := NewOpencodeProxy()
	body, _ := json.Marshal(map[string]any{
		"model": "chat-only", // 只收 chat
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.handleResponses(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "/v1/chat/completions") {
		t.Fatalf("responses 侧对只收 chat 的模型没有给出可操作提示：%d %s", w.Code, w.Body.String()[:200])
	}
}

// responses 形态的工具是扁平的（name 不嵌在 function 里），同样必须补官方核心工具。
func TestOpencodePrepareResponsesBody(t *testing.T) {
	raw := map[string]any{
		"model":  "space-bunny-free",
		"input":  []any{map[string]any{"role": "user", "content": "hi"}},
		"stream": false,
		"tools": []any{map[string]any{"type": "function", "name": "my_tool",
			"description": "d", "parameters": map[string]any{"type": "object"}}},
	}
	body := opencodePrepareResponsesBody(raw)
	if body["stream"] != true {
		t.Fatal("responses 也必须强制流式（上游非流式一律 403）")
	}
	tools, _ := body["tools"].([]any)
	got := map[string]bool{}
	for _, it := range tools {
		m := it.(map[string]any)
		n := opencodeToolName(m)
		got[n] = true
		// responses 形态：name 必须在顶层，不能嵌 function
		if _, nested := m["function"]; nested {
			t.Fatalf("工具 %q 用了 chat 的嵌套形态，responses 协议不认", n)
		}
	}
	for _, want := range opencodeCoreTools {
		if !got[want] {
			t.Fatalf("缺核心工具 %q", want)
		}
	}
	if !got["my_tool"] {
		t.Fatal("客户端自定义工具被丢掉")
	}
}

// 每个模型必须至少支持一种协议，否则它在本线路上根本不可用。
func TestOpencodeEveryModelReachableBySomeProtocol(t *testing.T) {
	for _, m := range opencodeFreeModels {
		if m.NoChat && m.NoResp {
			t.Errorf("%s 两种协议都不支持，不该挂进目录", m.ID)
		}
	}
}

// 上游只回状态码、body 全空时，转发给客户端的错误必须自己把话说完整。
// 2026-10-18 实测：opencode.ai 对 deepseek-v4.1-flash-free / step-5-preview-free
// 稳定返回 HTTP 500 且无响应体，原样转发会让 dsh 显示 {"message":""} —— 无从判断。
func TestUpstreamErrorWithEmptyBodyStillExplains(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("")),
	}
	p := &OpencodeProxy{log: NewFileLogger(filepath.Join(t.TempDir(), "oc.log"))}
	p.opencodeUpstreamError(rec, resp, "", "deepseek-v4.1-flash-free")

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON：%s", rec.Body.String())
	}
	msg := payload.Error.Message
	if strings.TrimSpace(msg) == "" {
		t.Fatalf("空 body 时 message 仍是空的，客户端无从判断：%s", rec.Body.String())
	}
	for _, want := range []string{"deepseek-v4.1-flash-free", "500"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息里缺 %q：%s", want, msg)
		}
	}
	if !strings.Contains(msg, "下架") && !strings.Contains(msg, "故障") {
		t.Errorf("没有给出可能原因（下架/上游故障）：%s", msg)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码应保持上游的 500，得到 %d", rec.Code)
	}
}

// 上游给了正经 message 时，别拿我们编的话覆盖掉它。
func TestUpstreamErrorKeepsRealMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body: io.NopCloser(strings.NewReader(
			`{"error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)),
	}
	p := &OpencodeProxy{log: NewFileLogger(filepath.Join(t.TempDir(), "oc.log"))}
	p.opencodeUpstreamError(rec, resp, "", "cat-jam-free")

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if !strings.Contains(payload.Error.Message, "free tier") {
		t.Errorf("上游原文被覆盖：%q", payload.Error.Message)
	}
	if !strings.Contains(payload.Error.Message, "免费档闸门") {
		t.Errorf("应有的中文提示没加上：%q", payload.Error.Message)
	}
}

// 用户 2026-10-18 定的规矩：oc 免费档只挂上下文窗口 ≥1M 的模型
// （"少于1M上下文的就不要了"）。以后往表里加模型时，这条会挡住漏标上下文的条目。
func TestOpencodeRosterRequires1MContext(t *testing.T) {
	if len(opencodeFreeModels) == 0 {
		t.Fatal("名单空了 —— 一个都不挂的话这条线路就没意义了")
	}
	const minContext = 1000000
	for _, m := range opencodeFreeModels {
		if m.Context < minContext {
			t.Errorf("%s 上下文 %d < 1M，按规矩不该挂在线路上", m.ID, m.Context)
		}
		if m.Context == 0 {
			t.Errorf("%s 没标上下文窗口（0 视为漏标，不是恰好为 0）", m.ID)
		}
	}
}
