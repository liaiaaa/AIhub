package main

// 国内凭证看板（卡片）的离线测试。
//
// 探活与额度查询都走注入的函数变量：不注入就得靠反复打上游来验证看板，
// 既耗额度又招风控（intl 那边同样的道理，见 rate_limit.go 的 probeInferFn）。

import (
	"encoding/json"
	"strings"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 造一个带凭据的国内凭证目录。字段形状对齐 auth.go 的 AccountData / 顶层 account。
func fakeDomCredDir(t *testing.T, name, uid, nick string) string {
	t.Helper()
	dir := filepath.Join(hubRoot, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"account": map[string]any{"uid": uid, "nickname": nick},
		"auth": map[string]any{
			"accessToken": "fake-token",
			"expiresAt":   time.Now().Add(24 * time.Hour).UnixMilli(),
		},
	}
	b, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop.info"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDomCredStatusUsesProbe(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	fakeDomCredDir(t, "甲凭证", "uid-dom-1", "zhang")

	probeCalls := 0
	oldProbe, oldCred := domProbeInferFn, domCreditsFn
	domProbeInferFn = func(cm *CredentialManager, p *Proxy) (probeResult, string) {
		probeCalls++
		return probeOK, ""
	}
	domCreditsFn = func(cm *CredentialManager, p *Proxy) (float64, float64, bool) {
		return 1200, 3000, true
	}
	t.Cleanup(func() { domProbeInferFn, domCreditsFn = oldProbe, oldCred })

	h := &Hub{}
	h.domCred = "甲凭证"
	domCredsCacheReset() // 进程级缓存会跨测试泄漏，先清（2026-10-20 缓存化后必须）
	list := domCredsStatus(h, true)
	if len(list) != 1 {
		t.Fatalf("应列出 1 个国内凭证，实际 %d：%+v", len(list), list)
	}
	c := list[0]
	if !c.OK || c.State != "可用" {
		t.Errorf("探活通过就该标可用：%+v", c)
	}
	if !c.Selected {
		t.Error("当前选中的凭证必须标 selected，卡片上的单选圈才点对")
	}
	if c.Remain != 1200 || c.Total != 3000 {
		t.Errorf("余额没带上：%+v", c)
	}
	if c.UID != "uid-dom-1" || c.Nick != "zhang" {
		t.Errorf("uid/nick 解析错：%+v", c)
	}
	if probeCalls != 1 {
		t.Errorf("探活调用 %d 次，应为 1 次", probeCalls)
	}
}

// 台账里有账号级记录时，直接给结论，不必再打上游（省额度、也避免刷新即探测）。
func TestDomCredStatusPrefersLedgerOverProbe(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	resetRateLimitIn(hubRoot)
	fakeDomCredDir(t, "乙凭证", "uid-dom-2", "li")

	rateLimitRecord("uid-dom-2", "li", rlKindAccount, "", 11140, "账号级风控(11140)", time.Time{})

	probed := 0
	oldProbe, oldCred := domProbeInferFn, domCreditsFn
	domProbeInferFn = func(cm *CredentialManager, p *Proxy) (probeResult, string) {
		probed++
		return probeOK, ""
	}
	domCreditsFn = func(cm *CredentialManager, p *Proxy) (float64, float64, bool) { return -1, -1, false }
	t.Cleanup(func() { domProbeInferFn, domCreditsFn = oldProbe, oldCred })

	domCredsCacheReset() // 进程级缓存会跨测试泄漏，先清（2026-10-20 缓存化后必须）
	c := domCredsStatus(&Hub{}, false)[0]
	if c.OK || c.State != "风控" {
		t.Fatalf("台账说风控就该标风控：%+v", c)
	}
	if probed != 0 {
		t.Errorf("台账已有结论还去打上游探活（%d 次），白耗额度", probed)
	}
}

// 账号通道通、个别模型被限：模型级限流要单独列出来（卡片上显示「哪个模型限到几点」）。
func TestDomCredStatusListsModelLimits(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	resetRateLimitIn(hubRoot)
	fakeDomCredDir(t, "丙凭证", "uid-dom-3", "wang")

	rateLimitRecord("uid-dom-3", "wang", rlKindModel, "glm-5.3", 6004, "用量限流(6004)", time.Now().Add(6*time.Hour))

	oldProbe, oldCred := domProbeInferFn, domCreditsFn
	domProbeInferFn = func(cm *CredentialManager, p *Proxy) (probeResult, string) { return probeOK, "" }
	domCreditsFn = func(cm *CredentialManager, p *Proxy) (float64, float64, bool) { return 10, 20, true }
	t.Cleanup(func() { domProbeInferFn, domCreditsFn = oldProbe, oldCred })

	domCredsCacheReset() // 进程级缓存会跨测试泄漏，先清（2026-10-20 缓存化后必须）
	c := domCredsStatus(&Hub{}, false)[0]
	if !c.OK {
		t.Fatalf("模型级限流不该把账号判死：%+v", c)
	}
	if len(c.ModelLimits) != 1 || c.ModelLimits[0].Model != "glm-5.3" {
		t.Fatalf("模型级限流没列出来：%+v", c.ModelLimits)
	}
	if !c.ModelLimits[0].UpstreamReset || c.ModelLimits[0].UpstreamUntil == "" {
		t.Errorf("上游给了恢复时刻就必须显示，别只写「限流」：%+v", c.ModelLimits[0])
	}
}

// 探活失败要带得出原因，不能全塌成「探活失败」四个字。
func TestDomCredStatusProbeFailureCarriesReason(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	fakeDomCredDir(t, "丁凭证", "uid-dom-4", "zhao")

	oldProbe, oldCred := domProbeInferFn, domCreditsFn
	domProbeInferFn = func(cm *CredentialManager, p *Proxy) (probeResult, string) {
		return probeUnknown, "网络错误：connection refused"
	}
	domCreditsFn = func(cm *CredentialManager, p *Proxy) (float64, float64, bool) { return -1, -1, false }
	t.Cleanup(func() { domProbeInferFn, domCreditsFn = oldProbe, oldCred })

	domCredsCacheReset() // 进程级缓存会跨测试泄漏，先清（2026-10-20 缓存化后必须）
	c := domCredsStatus(&Hub{}, false)[0]
	if c.OK {
		t.Fatal("探活失败不该标可用")
	}
	if c.Reason == "" || c.Reason == "探活失败" {
		t.Errorf("原因没带出来：%q", c.Reason)
	}
}

// 探活的请求头必须与推理路径同源：国内线路开了官方客户端伪装，探活漏了就是
// 「推理能用、看板全红」（2026-10-16 intl 踩过，见 intlProbeHeaders）。
//
// 2026-10-21 更新：dom 从「只补会话四头」升级为「整套官方客户端指纹」后，
// 断言随之改为「探活也必须是官方指纹」，并且直接比对两条路径的关键头——
// 这才是原测试真正想守住的东西（同源），比列举几个键更抗后续改动。
func TestDomProbeHeadersMatchInferencePath(t *testing.T) {
	p := NewDomProxyDir("")
	probe := p.domProbeHeaders(map[string]string{"Authorization": "Bearer t"})
	infer := p.withConversationHeaders(map[string]string{"Authorization": "Bearer t"})

	// 1) 探活必须带齐会话 id 与对话专项头
	// 键名大小写必须与 officialChatHeaders 写的一致（ID 全大写），
	// Go 的 map 键区分大小写，写成 "X-Conversation-Id" 会误判成缺失。
	for _, k := range []string{
		"X-Conversation-ID", "X-Conversation-Request-ID",
		"X-Conversation-Message-ID", "X-Request-ID",
		"X-Agent-Intent", "X-Agent-Type", "X-Agent-Purpose",
		"X-IDE-Name", "X-IDE-Type", "X-IDE-Version", "X-Private-Data",
	} {
		if probe[k] == "" {
			t.Errorf("国内探活头缺 %s", k)
		}
	}

	// 2) 探活 UA 必须与推理路径一致（都是官方内置 CLI 形态），
	//    不能是线路自己的 codebuddy2openai/2.0 —— 那正是被渠道校验拦的形状。
	if probe["User-Agent"] != infer["User-Agent"] {
		t.Errorf("探活 UA 与推理不一致：探活 %q / 推理 %q", probe["User-Agent"], infer["User-Agent"])
	}
	if !strings.HasPrefix(probe["User-Agent"], "WorkBuddy/") {
		t.Errorf("探活 UA 应为官方形态，得到 %q", probe["User-Agent"])
	}

	// 3) 两个开关都没开的线路不应被动过（其它线路保持原样）
	q := &Proxy{}
	if got := q.domProbeHeaders(map[string]string{}); len(got) != 2 {
		t.Errorf("未置位的线路被加了头：%v", got)
	}
}

// 探活模型必须在 dom 的可路由清单里，否则探测会被上游拒、把好凭证误判成不可用。
func TestDomProbeModelIsRoutable(t *testing.T) {
	p := NewDomProxyDir("")
	found := false
	for _, m := range p.Models {
		if m == domProbeModel {
			found = true
		}
	}
	if !found {
		t.Errorf("探活模型 %q 不在 dom 的 Models 清单里", domProbeModel)
	}
}

func resetRateLimitIn(dir string) {
	rateLimitState.mu.Lock()
	rateLimitState.loaded = false
	rateLimitState.entries = map[string]*rateLimitEntry{}
	rateLimitState.mu.Unlock()
	_ = os.WriteFile(filepath.Join(dir, rateLimitFile), []byte(`{"entries":{}}`), 0644)
}
