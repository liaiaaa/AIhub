package main

// 2026-10-20 23:00 腾讯给 WorkBuddy 对话网关加第三方客户端渠道校验：
//   400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}
// 对齐依据：yreidev/workbuddy2api-panel docs/official-client.md（解包
// WorkBuddy 5.8.3 安装包读出的拦截器实现）。这组测试钉住「对齐了哪些头」
// 以及「id 是 uuidv7 而不是 uuidv4」。

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOfficialChatHeadersComplete(t *testing.T) {
	in := map[string]string{
		"Authorization": "Bearer x",
		"X-User-Id":     "u1",
		"X-Domain":      "www.codebuddy.cn",
		"User-Agent":    "codebuddy2openai/2.0", // 旧值必须被覆盖
	}
	out := officialChatHeaders(in)

	// UA 必须换成官方内置 CLI 的形态
	wantUA := "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
	if out["User-Agent"] != wantUA {
		t.Errorf("UA 不对：%q，应为 %q", out["User-Agent"], wantUA)
	}
	// 认证相关头必须保留
	for _, k := range []string{"Authorization", "X-User-Id", "X-Domain"} {
		if out[k] != in[k] {
			t.Errorf("%s 被弄丢了：%q", k, out[k])
		}
	}
	// 公共头 + 对话专项头 + 链路头
	must := map[string]string{
		"Accept":           "application/json, text/plain, */*",
		"Content-Type":     "application/json",
		"Accept-Encoding":  "gzip, compress, deflate, br",
		"X-Requested-With": "XMLHttpRequest",
		"X-Product":        "SaaS",
		"X-Agent-Intent":   "craft",
		"X-Agent-Type":     "main",
		"X-Agent-Purpose":  "conversation",
		"X-IDE-Name":       "WorkBuddy",
		"X-IDE-Type":       "WorkBuddy",
		"X-IDE-Version":    "5.8.3",
		"X-Private-Data":   `"true"`, // 官方值带引号
		"X-B3-Sampled":     "1",
	}
	for k, want := range must {
		if out[k] != want {
			t.Errorf("%s = %q，应为 %q", k, out[k], want)
		}
	}
	// 会话三 id + 链路头的形状
	convRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !convRe.MatchString(out["X-Conversation-ID"]) {
		t.Errorf("X-Conversation-ID 应是带连字符的 uuidv7：%q", out["X-Conversation-ID"])
	}
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, k := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-B3-TraceId"} {
		if !hexRe.MatchString(out[k]) {
			t.Errorf("%s 应是 32 位十六进制：%q", k, out[k])
		}
	}
	if out["X-Conversation-Message-ID"] != out["X-Request-ID"] {
		t.Error("官方规定 Message-ID 与 X-Request-ID 同值（同一个模型消息 id）")
	}
	if !strings.HasPrefix(out["traceparent"], "00-") || !strings.HasSuffix(out["traceparent"], "-01") {
		t.Errorf("traceparent 形状不对：%q", out["traceparent"])
	}
	if out["b3"] == "" || !strings.HasSuffix(out["b3"], "-1-") {
		t.Errorf("b3 形状不对：%q", out["b3"])
	}
	// 官方明确不发这几个头
	for _, k := range []string{"Origin", "Referer", "X-Machine-ID", "X-Session-ID"} {
		if _, ok := out[k]; ok {
			t.Errorf("官方不发 %s，不该补", k)
		}
	}
}

// uuidv7 的前 48 位必须是毫秒时间戳，版本位 7 —— 上游若验 id 形状，v4 会露馅。
func TestUUIDv7Shape(t *testing.T) {
	now := time.Now()
	id := randUUIDv7(now)
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("不是标准 UUID 形态：%q", id)
	}
	if parts[2][0] != '7' {
		t.Errorf("版本位应为 7：%q", parts[2])
	}
	if !strings.ContainsRune("89ab", rune(parts[3][0])) {
		t.Errorf("变体位应为 8/9/a/b：%q", parts[3])
	}
	// 前 48 位 = 时间戳（允许 2ms 内误差）
	hexTS := parts[0] + parts[1]
	ms, err := strconv.ParseInt(hexTS, 16, 64)
	if err != nil {
		t.Fatalf("时间戳段不是十六进制：%q", hexTS)
	}
	diff := ms - now.UnixMilli()
	if diff < 0 {
		diff = -diff
	}
	if diff > 2 {
		t.Errorf("前 48 位应是毫秒时间戳，差 %dms", diff)
	}
	if h := uuidv7Hex(now); len(h) != 32 || strings.Contains(h, "-") {
		t.Errorf("uuidv7Hex 应是去连字符 32 位：%q", h)
	}
}

// dom 用官方整套头；intl 保持原有「只补会话四头」的行为，不被这次改动波及。
func TestOfficialClientOnlyOnDom(t *testing.T) {
	dom := NewDomProxyDir(t.TempDir())
	if !dom.OfficialChatClient {
		t.Error("dom 线路应启用 OfficialChatClient")
	}
	out := dom.withConversationHeaders(map[string]string{"User-Agent": "old"})
	if out["X-Agent-Intent"] != "craft" {
		t.Error("dom 应补齐对话专项头")
	}
	if !strings.HasPrefix(out["User-Agent"], "WorkBuddy/") {
		t.Errorf("dom 的 UA 应换成官方形态：%q", out["User-Agent"])
	}

	intl := &Proxy{ConversationHeaders: true} // 模拟 intl 配置
	out2 := intl.withConversationHeaders(map[string]string{"User-Agent": "codebuddy-guoji/1.0"})
	if out2["User-Agent"] != "codebuddy-guoji/1.0" {
		t.Error("intl 的 UA 不该被改")
	}
	if _, ok := out2["X-Agent-Intent"]; ok {
		t.Error("intl 不该补对话专项头（它此前已正常工作，不做顺手统一）")
	}
	if out2["X-Conversation-ID"] == "" {
		t.Error("intl 仍应补会话四头")
	}
}

// 探活走的是 domProbeHeaders → withConversationHeaders，必须同样吃到官方头，
// 否则会出现「推理能用、看板全红」的假故障（intl 2026-10-16 踩过）。
func TestDomProbeHeadersUsesOfficial(t *testing.T) {
	p := NewDomProxyDir(t.TempDir())
	h := p.domProbeHeaders(map[string]string{"User-Agent": "old", "Authorization": "Bearer x"})
	if h["X-Agent-Purpose"] != "conversation" {
		t.Error("探活也要带对话专项头")
	}
	if !strings.HasPrefix(h["User-Agent"], "WorkBuddy/") {
		t.Errorf("探活 UA 也要换成官方：%q", h["User-Agent"])
	}
}
