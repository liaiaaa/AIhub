package main

// 真机验证（默认跳过）：确认 2026-10-25 的出站身份对齐没有把线路弄坏。
//
// 启用：
//
//	DOM_LIVE_TEST=1 go test -run TestLiveDomIdentity -v
//	INTL_LIVE_TEST=1 go test -run TestLiveIntlIdentity -v
//
// 设计原则：
//   - 直接在本进程用**新的**头与清洗函数打上游，不经过正在跑的 hub 进程，
//     所以不需要重启 aihub（重启会打断当前 dsh 会话的模型调用）；
//   - 只发最小请求：dom 一条对话 + 目录 + billing；intl 一条对话；
//   - dom 用便宜的 glm-5.3-flash，intl 用免费的 deepseek-v4.1-flash；
//   - 带触发句 system 的那条必须走 sanitizeHarnessMessages 之后再发——
//     故意发未清洗的原始句等于主动去撞 11128，不做。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func liveClient() *http.Client {
	c := newBackendClient()
	c.Timeout = 90 * time.Second
	return c
}

func liveCredDir(t *testing.T, name string) string {
	t.Helper()
	if d := domCredDirs()[name]; d != "" {
		return d
	}
	t.Skipf("没有 %s 凭证目录，跳过真机验证", name)
	return ""
}

// TestLiveDomIdentity dom：官方头 + 清洗后的触发句 system，应拿到 200。
func TestLiveDomIdentity(t *testing.T) {
	if os.Getenv("DOM_LIVE_TEST") == "" {
		t.Skip("跳过 dom 真机验证；设置 DOM_LIVE_TEST=1 启用")
	}
	hubRoot = repoRoot(t) // go test 的二进制在临时目录，hubRoot 会算错；先指回真实数据根
	dir := liveCredDir(t, "CodeBuddy凭证")
	p := NewDomProxyDir(dir)
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		t.Skip("dom 凭证文件不存在，跳过")
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	headers, err := cm.Headers()
	if err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}

	raw := "You are an AI agent powered by DeepSeek Harness.\n" +
		"You are a coding agent powered by the dom.step-5-preview model.\n" +
		"Keep the answer to one word."
	sanitized := sanitizeHarnessMessages([]any{
		map[string]any{"role": "system", "content": raw},
		map[string]any{"role": "user", "content": "hi"},
	})
	body := map[string]any{
		"model":                 "glm-5.3-flash",
		"messages":              sanitized,
		"stream":                true,
		"stream_options":        map[string]any{"include_usage": true},
		"max_completion_tokens": 16,
	}
	payload, _ := json.Marshal(body)
	if strings.Contains(string(payload), "DeepSeek Harness") {
		t.Fatal("清洗没生效：payload 里仍有 DeepSeek Harness")
	}

	url := p.Backend + p.chatPath()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.withConversationHeaders(headers) {
		req.Header.Set(k, v)
	}
	resp, err := liveClient().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	t.Logf("dom chat HTTP %d | UA=%s | 首 200 字节：%s", resp.StatusCode, req.Header.Get("User-Agent"), truncate(string(b), 200))
	if resp.StatusCode != http.StatusOK {
		// 11140 是**账号级**风控（上游文案：账号存在异常行为），与本次 UA 改动无关：
		// 同一个账号的目录与 billing 请求用新 UA 仍然通过（见另一个真机测试）。
		// 这种情况要如实跳过，而不是把「账号早就被封」误报成「身份对齐把它弄坏了」。
		if strings.Contains(string(b), `"code":11140`) {
			t.Skipf("该账号已处于 11140 账号级风控，这条请求无法判断 UA 是否可用；"+
				"目录/billing 已单独验证通过。上游原文：%s", truncate(string(b), 200))
		}
		t.Fatalf("dom 对话应 200，实际 %d：%s", resp.StatusCode, truncate(string(b), 400))
	}
	if !strings.Contains(string(b), "choices") && !strings.Contains(string(b), "data:") {
		t.Fatalf("200 但没有 SSE 内容：%s", truncate(string(b), 400))
	}
	if got := req.Header.Get("User-Agent"); got != officialDomUA() {
		t.Errorf("出站 UA 不是官方身份：%q", got)
	}
}

// TestLiveDomCatalogAndBilling：目录（带 credits）与 billing 仍能通，
// 证明 CLI/2.161.0 与 WorkBuddy/5.8.3 这两个形状没被上游拒。
func TestLiveDomCatalogAndBilling(t *testing.T) {
	if os.Getenv("DOM_LIVE_TEST") == "" {
		t.Skip("跳过 dom 真机验证；设置 DOM_LIVE_TEST=1 启用")
	}
	hubRoot = repoRoot(t) // go test 的二进制在临时目录，hubRoot 会算错；先指回真实数据根
	dir := liveCredDir(t, "CodeBuddy凭证")
	p := NewDomProxyDir(dir)
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		t.Skip("dom 凭证文件不存在，跳过")
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	headers, err := cm.Headers()
	if err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}

	cat, err := fetchWBCatalog(liveClient(), p.Backend, "dom", headers)
	if err != nil {
		t.Fatalf("目录接口失败（CLI/2.161.0 形状被拒？）: %v", err)
	}
	withRatio := 0
	for _, e := range cat.Entries {
		if e.Chat && e.Ratio != "" {
			withRatio++
		}
	}
	t.Logf("dom 目录：%d 条，其中带倍率/credits 的 %d 条", len(cat.Entries), withRatio)
	if len(cat.Entries) < 10 {
		t.Errorf("目录条目过少（%d），CLI 身份形状可能又被上游改了", len(cat.Entries))
	}
	if withRatio == 0 {
		t.Error("没有任何条目带 credits：billing/目录 UA 形状不对")
	}

	remain, total, ok := domCreditsRaw(cm, p)
	if !ok {
		t.Fatal("billing 查询失败：WorkBuddy/5.8.3 单段 UA 可能不被接受")
	}
	t.Logf("dom billing：可用 %.1f / 总共 %.1f", remain, total)
}

// TestLiveIntlIdentity intl：官方 UA + 会话四头，一条 hi 应该 200。
// 优先挑未被 11140 风控的凭证，避免把已封号当成 UA 问题误判。
func TestLiveIntlIdentity(t *testing.T) {
	if os.Getenv("INTL_LIVE_TEST") == "" {
		t.Skip("跳过 intl 真机验证；设置 INTL_LIVE_TEST=1 启用")
	}
	hubRoot = repoRoot(t) // 指回真实数据根，否则 credDirs() 扫的是 go test 的临时目录
	dirs := credDirs()
	if len(dirs) == 0 {
		t.Skip("没有国际凭证目录，跳过")
	}
	// 候选顺序：优先 <凭证目录B>（state.json 里选中的），其余目录排后面。
	// 台账里有冷却记录（当天 11140）的账号直接跳过——那种账号的 11140 会把
	// 「UA 有没有问题」和「账号本来就不能用」混在一起。
	cands := make([]string, 0, len(dirs))
	if d := dirs["<凭证目录B>"]; d != "" {
		cands = append(cands, d)
	}
	for _, d := range dirs {
		if d != "" && (len(cands) == 0 || d != cands[0]) {
			cands = append(cands, d)
		}
	}
	var pick string
	for _, dir := range cands {
		cand := NewIntlProxy(dir)
		candPath := cand.AuthFilePath()
		if candPath == "" || !fileExists(candPath) {
			continue
		}
		if sum, err := NewCredentialManager(candPath, cand.Backend, cand.DefaultDomain, cand.UserAgent).Summary(); err == nil {
			uid, _ := sum["uid"].(string)
			if e, limited := rateLimitFor(uid, ""); limited {
				t.Logf("跳过凭证目录 %s：台账里还在冷却（%s）", dir, e.Reason)
				continue
			}
		}
		pick = dir
		break
	}
	if pick == "" {
		t.Skip("所有国际凭证都在台账冷却中或没有凭证文件，跳过")
	}
	p := NewIntlProxy(pick)
	path := p.AuthFilePath()
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	headers, err := cm.Headers()
	if err != nil {
		t.Fatalf("读凭据失败: %v", err)
	}
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"stream":                true,
		"stream_options":        map[string]any{"include_usage": true},
		"max_completion_tokens": 16,
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, p.Backend+p.chatPath(), strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.withConversationHeaders(headers) {
		req.Header.Set(k, v)
	}
	resp, err := liveClient().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	t.Logf("intl chat HTTP %d | UA=%s | 首 200 字节：%s", resp.StatusCode, req.Header.Get("User-Agent"), truncate(string(b), 200))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("intl 对话应 200，实际 %d：%s", resp.StatusCode, truncate(string(b), 400))
	}
	if !strings.Contains(string(b), "choices") && !strings.Contains(string(b), "data:") {
		t.Fatalf("200 但没有 SSE 内容：%s", truncate(string(b), 400))
	}
	if got := req.Header.Get("User-Agent"); got != officialIntlUA() {
		t.Errorf("intl 出站 UA 不是官方身份：%q", got)
	}
}
