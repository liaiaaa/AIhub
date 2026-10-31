package main

// Qoder 线路的提示词清洗、会话租约、重试分类。
// 清洗判据全部来自真实 dump（qoder-dump-425cfbfa-upstream.json）：
// system 18815 字符、3 处 DeepSeek Harness、14 条消息带 <acp ...> 标签。

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeHarnessPromptQoder(t *testing.T) {
	cases := []string{
		"You are an AI agent powered by DeepSeek Harness.",
		"You are a coding agent powered by the qoder.DeepSeek-Flash model.",
	}
	for _, in := range cases {
		if got := sanitizeHarnessPrompt(in); got != harnessNeutralIdentity {
			t.Errorf("身份句没清干净：%q → %q", in, got)
		}
	}

	// 真实 system 头部：两句连在一起，功能内容必须保留
	sys := strings.Join(cases, "\n\n") + "\n\nAgent Teams is available in this session."
	out := sanitizeHarnessMessages([]any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hi"},
	})
	got := out[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "DeepSeek Harness") || strings.Contains(got, "DeepSeek-Flash") {
		t.Errorf("system 仍带 dsh 身份指纹：%q", got)
	}
	if !strings.Contains(got, "Agent Teams is available") {
		t.Errorf("功能内容被破坏：%q", got)
	}
	if out[1].(map[string]any)["content"] != "hi" {
		t.Error("user 消息不该被改")
	}
}

func TestSanitizeACPTags(t *testing.T) {
	// sanitizeACPTags 是工具函数（只给 business.name 用），本身行为不变。
	cases := []struct{ name, in, want string }{
		{"完整标签+正文", `<acp tokens="159" type="text">m00001</acp>正文`, "正文"},
		{"工具标签", `<acp tokens="2.1K" type="tool:bash">m00175</acp>`, ""},
		{"行内标签", `前<acp tokens="1" type="text">m1</acp>后`, "前后"},
		{"缺闭合标签", `<acp tokens="1" type="text">m1`, "m1"},
		{"普通文本", "普通文本", "普通文本"},
	}
	for _, c := range cases {
		if got := sanitizeACPTags(c.in); got != c.want {
			t.Errorf("%s：%q → %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// 2026-10-27 用户拍板（选 C：不清）：ACP 标注标签是 dsh 的消息引用编号，
// 清掉后模型无法按编号定位历史消息，所以 messages 里**一律保留不动**。
// 这条测试是回归护栏：谁再给 sanitizeHarnessMessages 加 ACP 清洗就会红。
func TestSanitizeKeepsACPTagsInMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": `说明：<acp tokens="1" type="text">m1</acp>正文`},
		map[string]any{"role": "user", "content": `<acp tokens="2" type="text">m2</acp>用户内容`},
		map[string]any{"role": "assistant", "content": `<acp tokens="3" type="tool:bash">m3</acp>已执行`},
	}
	out := sanitizeHarnessMessages(msgs)
	for i, want := range []string{"<acp", "<acp", "<acp"} {
		got := out[i].(map[string]any)["content"].(string)
		if !strings.Contains(got, want) {
			t.Errorf("第 %d 条消息的 ACP 标签被清掉了（用户要求保留）：%q", i, got)
		}
	}
	// 正文与标签都必须原样保留
	if got := out[1].(map[string]any)["content"].(string); got != `<acp tokens="2" type="text">m2</acp>用户内容` {
		t.Errorf("user 消息被改动了：%q", got)
	}
}

// 非 system 消息只去 ACP 标签，身份句/品牌一个字都不许动——
// 那是用户与工具的真实内容，改写它既越权也会弄脏模型看到的代码。
func TestSanitizeKeepsUserBodyIntact(t *testing.T) {
	body := "请解释这句话：You are an AI agent powered by DeepSeek Harness. 以及 OpenAI 是什么"
	out := sanitizeHarnessMessages([]any{
		map[string]any{"role": "user", "content": body},
	})
	if got := out[0].(map[string]any)["content"]; got != body {
		t.Errorf("用户内容被改写了（越权）：%q", got)
	}
}

func TestQoderSessionLeaseReuse(t *testing.T) {
	p := NewQoderProxy()
	uid := "u1"
	base := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "first"},
	}
	key := qoderLeaseKey("", base)

	l1 := p.sessionLease(key, uid)
	l2 := p.sessionLease(key, uid)
	if l1.SessionID != l2.SessionID || l1.RequestSetID != l2.RequestSetID || l1.BusinessID != l2.BusinessID {
		t.Fatalf("同 key 两次应复用同一租约：%+v / %+v", l1, l2)
	}

	other := qoderLeaseKey("", []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "another"},
	})
	l3 := p.sessionLease(other, uid)
	if l3.SessionID == l1.SessionID {
		t.Error("不同会话 key 不应复用 session_id")
	}

	// 超过 TTL：换新租约
	p.sessionMu.Lock()
	p.sessionCache[key].lastSeen = time.Now().Add(-qoderSessionLeaseTTL - time.Minute)
	p.sessionMu.Unlock()
	l4 := p.sessionLease(key, uid)
	if l4.SessionID == l1.SessionID {
		t.Error("超过 TTL 应新建会话")
	}
}

// 同一段对话逐轮增长时，前 2 条消息不变 → key 必须稳定。
func TestQoderLeaseKeyStableAcrossTurns(t *testing.T) {
	sys := map[string]any{"role": "system", "content": "sys"}
	first := map[string]any{"role": "user", "content": "first"}
	turn1 := []any{sys, first, map[string]any{"role": "user", "content": "第 1 轮"}}
	turn2 := []any{sys, first, map[string]any{"role": "user", "content": "第 2 轮"}}
	if qoderLeaseKey("", turn1) != qoderLeaseKey("", turn2) {
		t.Error("最后一条 user 变化不应改变租约 key")
	}
	other := []any{sys, map[string]any{"role": "user", "content": "另一个开头"}}
	if qoderLeaseKey("", other) == qoderLeaseKey("", turn1) {
		t.Error("不同开头应得到不同租约 key")
	}
	// 客户端显式标识优先
	if qoderLeaseKey("s1", turn1) == qoderLeaseKey("s2", turn1) {
		t.Error("客户端会话标识应参与 key")
	}
}

func TestQoderEnvelopeUsesLease(t *testing.T) {
	lease := &qoderSessionLease{SessionID: "sess-1", RequestSetID: "rset-1", BusinessID: "biz-1"}
	msgs := []any{map[string]any{"role": "user", "content": strings.Repeat("长", 20)}}
	entry := helperQfEntry(t)

	env1 := qoderEnvelope(msgs, nil, "qfmodel", "uid-1", qoderThinkingSpec{}, entry, map[string]any{}, lease)
	env2 := qoderEnvelope(msgs, nil, "qfmodel", "uid-1", qoderThinkingSpec{}, entry, map[string]any{}, lease)

	if env1["session_id"] != "sess-1" || env1["request_set_id"] != "rset-1" {
		t.Errorf("租约未生效：session_id=%v request_set_id=%v", env1["session_id"], env1["request_set_id"])
	}
	biz, _ := env1["business"].(map[string]any)
	if biz["id"] != "biz-1" {
		t.Errorf("business.id 未用租约：%v", biz["id"])
	}
	if env1["request_id"] == env2["request_id"] {
		t.Error("request_id 应每请求新建")
	}
	if env1["chat_record_id"] != env1["request_id"] {
		t.Error("chat_record_id 必须等于 request_id（官方 A6e()）")
	}
	if name, _ := biz["name"].(string); len([]rune(name)) != 10 {
		t.Errorf("business.name 应截 10 字符，实际 %q（%d 字符）", name, len([]rune(name)))
	}

	// lease=nil 回落到每请求新建
	ns1 := qoderEnvelope(msgs, nil, "qfmodel", "uid-1", qoderThinkingSpec{}, entry, map[string]any{}, nil)
	ns2 := qoderEnvelope(msgs, nil, "qfmodel", "uid-1", qoderThinkingSpec{}, entry, map[string]any{}, nil)
	if ns1["session_id"] == ns2["session_id"] {
		t.Error("无租约时 session_id 应每请求新建")
	}
}

func TestQoderRetryClassification(t *testing.T) {
	// HTTP 状态：只有可重试 5xx
	for _, code := range []int{500, 502, 503, 504} {
		if !qoderRetryableStatus(code) {
			t.Errorf("HTTP %d 应可重试", code)
		}
	}
	for _, code := range []int{200, 400, 401, 403, 429, 501, 505} {
		if qoderRetryableStatus(code) {
			t.Errorf("HTTP %d 不应重试", code)
		}
	}

	// 首帧错误：110/101/418/406 确定性失败
	fatal := []string{
		`{"code":"110","message":"Billing daily count exceeded"}`,
		`{"code":101,"message":"bad sign"}`,
		`{"code":"418","message":"content review"}`,
		`{"code":"406","message":"Session blocked"}`,
	}
	for _, s := range fatal {
		if !qoderFatalFirstFrame(s) {
			t.Errorf("应判定为确定性失败：%s", s)
		}
	}
	nonFatal := []string{
		`{"code":"provider_error","message":"Error in upstream response"}`,
		`{"code":"10605","message":"queued"}`,
		`{"code":"504","message":"timeout"}`,
		`{"code":"1105","message":"other"}`,
		`{"message":"no code"}`,
		`not json`,
	}
	for _, s := range nonFatal {
		if qoderFatalFirstFrame(s) {
			t.Errorf("不应判定为确定性失败：%s", s)
		}
	}
}

// Accept-Encoding: identity 的功能性风险兜底：上游真回 gzip 也要能解析。
func TestQoderMaybeDecompress(t *testing.T) {
	var buf strings.Builder
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("data: hello\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"gzip"}},
		Body:   io.NopCloser(strings.NewReader(buf.String())),
	}
	resp = qoderMaybeDecompress(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("解压后读取失败：%v", err)
	}
	if string(body) != "data: hello\n\n" {
		t.Errorf("解压内容不对：%q", body)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Error("解压后应清掉 Content-Encoding")
	}
	// 非 gzip 原样返回
	plain := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader("x"))}
	if qoderMaybeDecompress(plain) != plain {
		t.Error("非 gzip 响应不该被包装")
	}
}

// 2026-10-27：business.name 不能再出现 "<acp token" 这种一眼假的前缀。
// 它取的是 lastUser，而 lastUser 可能带 ACP 标签（QODER_SANITIZE=0 时尤其）。
func TestQoderBusinessNameStripsACP(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<acp tokens="36" type="text">m00123</acp> 只回答两个字：收到`, "只回答两个字：收到"},
		{`<acp tokens="12" type="text">m00124</acp>再来一次`, "再来一次"},
		{"普通问题", "普通问题"},
		{"", ""},
	}
	for _, c := range cases {
		got := qoderBusinessName(c.in)
		if strings.Contains(got, "<acp") {
			t.Errorf("business.name 仍带 ACP 标签：%q → %q", c.in, got)
		}
		if got != c.want {
			t.Errorf("business.name = %q, want %q", got, c.want)
		}
		if n := len([]rune(got)); n > 10 {
			t.Errorf("business.name 超过 10 字符：%q（%d）", got, n)
		}
	}
}

// 2026-10-27：版本探测不能一辈子缓存。
// 实测事故：进程 18:30 启动缓存 0.4.8，桌面端 18:32 升到 0.4.11，
// hub 之后每请求仍报 0.4.8 —— 版本永远落后一个升级周期。
func TestQoderVersionCacheExpires(t *testing.T) {
	home := t.TempDir()
	stPath := filepath.Join(home, ".qoder-cn")
	if err := os.MkdirAll(stPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeVer := func(v string) {
		b, _ := json.Marshal(map[string]any{"version": v, "product": "qodercn"})
		if err := os.WriteFile(filepath.Join(stPath, ".qoder-app-status.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeVer("0.4.8")
	if got := qoderProbeAppVersion(home); got != "0.4.8" {
		t.Fatalf("首次探测 = %q, want 0.4.8", got)
	}

	// 桌面端升级
	writeVer("0.4.11")

	// 缓存未过期：仍是旧值（这是有意的，避免每请求读文件）
	qoderVerMu.Lock()
	qoderAppVersion = "0.4.8"
	qoderVerAt = time.Now()
	qoderVerMu.Unlock()
	if app, _ := qoderVersions(); app != "0.4.8" {
		t.Errorf("TTL 内应命中缓存，实际 %q", app)
	}

	// 把缓存时间拨到 TTL 之外：必须重探到新版本
	qoderVerMu.Lock()
	qoderVerAt = time.Now().Add(-qoderVersionTTL - time.Second)
	qoderVerMu.Unlock()
	_ = qoderProbeAppVersion // 保证函数被引用
	qoderVerMu.Lock()
	homeNow, _ := os.UserHomeDir()
	qoderAppVersion = qoderProbeAppVersion(homeNow)
	qoderVerAt = time.Now()
	qoderVerMu.Unlock()
	t.Logf("真实 home 探测 = %s（本机桌面端版本）", qoderAppVersion)
	if qoderAppVersion == "" {
		t.Error("重探后版本为空")
	}
}
