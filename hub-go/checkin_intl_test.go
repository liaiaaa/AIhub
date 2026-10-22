package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 国际版「每日活跃上报」（2026-10-12）。
//
// 关键回归点：事件**必须带 userId** —— 开源实现明确「缺失则服务端 200 但静默
// 丢弃」，这正是用户「次日看不到 30 积分到账」却毫无报错的原因。
func TestIntlChatRequestEventShape(t *testing.T) {
	const uid = "00000000-0000-4000-8000-000000000000"
	ev := intlChatRequestEvent(uid, "deepseek-v4.1-flash", "DeepSeek V4.1 Flash")

	if ev["userId"] != uid {
		t.Fatalf("userId 必须带且等于账号 uid（否则 200 静默丢弃）：%v", ev["userId"])
	}
	if ev["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode 应为 chat_request_send：%v", ev["eventCode"])
	}
	// 这几个是服务端判定活跃的必要字段
	for _, k := range []string{"timestamp", "mode", "conversationId", "requestId",
		"requestModelId", "requestModelName", "presentAt", "agentType"} {
		if _, ok := ev[k]; !ok {
			t.Fatalf("缺少字段 %s：%v", k, ev)
		}
	}
	if ev["mode"] != "craft" {
		t.Fatalf("mode 应为 craft：%v", ev["mode"])
	}
	// 三个 ID 应一致（同一条上报），且非空
	cid, _ := ev["conversationId"].(string)
	rid, _ := ev["requestId"].(string)
	root, _ := ev["rootRequestId"].(string)
	if cid == "" || cid != rid || cid != root {
		t.Fatalf("conversationId/requestId/rootRequestId 应一致且非空：%q %q %q", cid, rid, root)
	}
	if !strings.HasPrefix(cid, "hub-go-active-") {
		t.Fatalf("conversationId 前缀可辨识来源：%q", cid)
	}
	// 数值字段不能被序列化成字符串（服务端可能强类型校验）
	if _, ok := ev["inputLength"].(int); !ok {
		t.Fatalf("inputLength 应为数值：%T", ev["inputLength"])
	}
	if _, ok := ev["timestamp"].(int64); !ok {
		t.Fatalf("timestamp 应为毫秒整数：%T", ev["timestamp"])
	}
	// 数组字段必须是数组（不能是 nil，否则 JSON 出 null）
	for _, k := range []string{"mentionContexts", "knowledgeId", "knowledgeName"} {
		if _, ok := ev[k].([]any); !ok {
			t.Fatalf("%s 应为数组：%T", k, ev[k])
		}
	}
	// 必须能序列化成 JSON 数组的一个元素（/v2/report 的 body 形态）
	b, err := json.Marshal([]any{ev})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !strings.HasPrefix(string(b), "[{") {
		t.Fatalf("/v2/report body 应为 JSON 数组：%s", b[:60])
	}
}

// intlCheckin 在凭据缺失时不应 panic，且给出可读原因
func TestIntlCheckinMissingCred(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	got := intlCheckin("不存在的凭证")
	if got.OK {
		t.Fatalf("凭据不存在时不该报成功：%+v", got)
	}
	if got.Detail == "" {
		t.Fatalf("应给出可读原因")
	}
	t.Logf("提示文案：%s", got.Detail)
}

// 端到端：走真实 intlCheckin 逻辑，桩上游验证请求体形态
func TestIntlCheckinPostsEventWithUserID(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	// 造一个最小可用的国际凭证目录
	dir := dataPath("测试凭证")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cred := `{"auth":{"accessToken":"a.b.c","domain":"www.workbuddy.ai","tokenType":"Bearer",
	  "expiresAt":9999999999999},"account":{"uid":"uid-1234","nickname":"tester"}}`
	if err := os.WriteFile(dataPath("测试凭证", "workbuddy-desktop-ai.info"), []byte(cred), 0600); err != nil {
		t.Fatal(err)
	}

	var got []map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("路径应为 /v2/report，实际 %s", r.URL.Path)
		}
		if uid := r.Header.Get("X-User-Id"); uid != "uid-1234" {
			t.Errorf("请求头 X-User-Id 应为 uid-1234，实际 %q", uid)
		}
		var arr []map[string]any
		json.NewDecoder(r.Body).Decode(&arr)
		got = arr
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer up.Close()

	// 用桩替换真实域名
	oldURL := intlReportURL
	intlReportURL = up.URL + "/v2/report"
	defer func() { intlReportURL = oldURL }()

	res := intlCheckin("测试凭证")
	if !res.OK {
		t.Fatalf("应上报成功：%+v", res)
	}
	if len(got) != 1 {
		t.Fatalf("应发 1 条事件，实际 %d", len(got))
	}
	if got[0]["userId"] != "uid-1234" {
		t.Fatalf("事件 userId 应为 uid-1234：%v", got[0]["userId"])
	}
	t.Logf("上报结果文案：%s", res.Detail)
}

// 上游返回 code!=0 时应报失败，而不是假成功
func TestIntlCheckinBusinessError(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	dir := dataPath("测试凭证")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cred := `{"auth":{"accessToken":"a.b.c","domain":"www.workbuddy.ai","expiresAt":9999999999999},
	  "account":{"uid":"uid-1","nickname":"t"}}`
	if err := os.WriteFile(dataPath("测试凭证", "workbuddy-desktop-ai.info"), []byte(cred), 0600); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":500,"msg":"risk control"}`))
	}))
	defer up.Close()
	oldURL := intlReportURL
	intlReportURL = up.URL + "/v2/report"
	defer func() { intlReportURL = oldURL }()

	res := intlCheckin("测试凭证")
	if res.OK {
		t.Fatalf("code!=0 不该报成功：%+v", res)
	}
	t.Logf("错误文案：%s", res.Detail)
}
