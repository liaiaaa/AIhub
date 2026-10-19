package main

// 目录解析的离线测试。样本 testdata/wb_config_intl.json 取自 2026-10-16 真实的
// GET https://www.workbuddy.ai/v3/config，只保留 data.models 且字段裁剪到解析用到的
// 那几个，所以不含任何凭据或个人信息。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wbTestSample(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "wb_config_intl.json"))
	if err != nil {
		t.Skip("没有真实样本，跳过：", err)
	}
	return b
}

func TestParseWBCredits(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"x0.79 credits", "0.79x"},
		{"x0.00", "免费"},
		{"x3.31 credits", "3.31x"},
		{"0.06", "0.06x"},
		{"", ""},          // 上游没给就别硬编
		{"no-number", ""}, // 解析不出来也不硬编
	} {
		if got := parseWBCredits(tc.in); got != tc.want {
			t.Errorf("parseWBCredits(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestParseWBConfigBodyRealSample(t *testing.T) {
	cat, err := parseWBConfigBody(wbTestSample(t), "intl")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 27 {
		t.Fatalf("样本里 27 个模型，解析出 %d 个", len(cat.Entries))
	}
	by := cat.byID()

	// 上下文取 supportedLengths 的最大值，不是默认档位 —— 客户端要按能配到的上限报。
	ds := by["deepseek-v4.1-flash"]
	if ds.Context != 1000000 {
		t.Errorf("deepseek-v4.1-flash 上下文应取到 1M（supportedLengths 里的大值），得到 %d", ds.Context)
	}
	if ds.Default != 300000 {
		t.Errorf("默认档应是 300000，得到 %d", ds.Default)
	}
	if ds.Ratio != "免费" {
		t.Errorf("上游 credits=x0.00 应显示「免费」，得到 %q", ds.Ratio)
	}
	if by["glm-5.2"].Ratio != "0.79x" {
		t.Errorf("glm-5.2 倍率应是 0.79x，得到 %q", by["glm-5.2"].Ratio)
	}
	if by["step-5-preview"].Desc == "" {
		t.Error("step-5-preview 应有中文描述")
	}

	// 图像/视频模型没有输入上限，必须标 Chat=false —— 混进对话目录客户端选了必挂。
	nChat := 0
	for _, e := range cat.Entries {
		if e.Chat {
			nChat++
		} else if e.ID != "gpt-image-2.5-sunburst" && e.ID != "seedance-2.5" {
			t.Errorf("%s 被判成非对话模型，但样本里它有输入上限？", e.ID)
		}
	}
	if nChat != 25 {
		t.Errorf("对话模型应为 25 个，实际 %d", nChat)
	}
}

func TestParseWBConfigBodyBusinessCode(t *testing.T) {
	if _, err := parseWBConfigBody([]byte(`{"code":12403,"msg":"check ua"}`), "intl"); err == nil {
		t.Fatal("业务码非 0 必须报错，不能当成空目录")
	}
	// code=0 但 models 空 = 上游改结构，也要报错，别让目录悄悄变空。
	if _, err := parseWBConfigBody([]byte(`{"code":0,"data":{"models":[]}}`), "intl"); err == nil {
		t.Fatal("解析出 0 个模型必须报错")
	}
}

func TestWBCatalogDiskRoundTrip(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	wbCatalogMu.Lock()
	wbCatalogCache = map[string]*wbCatalog{}
	wbCatalogMu.Unlock()
	t.Cleanup(func() {
		hubRoot = old
		wbCatalogMu.Lock()
		wbCatalogCache = map[string]*wbCatalog{}
		wbCatalogMu.Unlock()
	})

	cat, err := parseWBConfigBody(wbTestSample(t), "intl")
	if err != nil {
		t.Fatal(err)
	}
	wbCatalogPut(cat)

	if e, ok := wbCatalogLookup("intl", "glm-5.2"); !ok || e.Ratio != "0.79x" {
		t.Fatalf("内存查询失败：%v %v", ok, e)
	}
	// 清掉内存，只留磁盘 —— 线路没启动时也得能显示倍率/上下文，靠的就是这条。
	wbCatalogMu.Lock()
	wbCatalogCache = map[string]*wbCatalog{}
	wbCatalogMu.Unlock()
	if e, ok := wbCatalogLookup("intl", "glm-5.2"); !ok || e.Context != 1000000 {
		t.Fatalf("磁盘快照查询失败：%v %v", ok, e)
	}
	if _, ok := wbCatalogLookup("dom", "glm-5.2"); ok {
		t.Error("dom 没有快照，不该查到东西")
	}
	if wbCatalogContext("intl", "不存在的模型") != 0 {
		t.Error("查不到必须返回 0，让调用方回退静态表")
	}
}

// 目录接口按 UA 决定响应形状，这是最容易踩的坑，2026-10-19 踩过一次。
//
// 实测矩阵（同一凭据）：
//
//	copilot.tencent.com（dom）
//	  UA=CodeBuddy/5.7.6               → data 只剩 enterpriseId/productFeatures，无 models
//	  UA=WorkBuddy/5.7.6               → models=37，**credits 一个都没有**
//	  UA=CLI/2.63.2 CodeBuddy/2.63.2   → models=34，**credits 32 个**
//
//	www.workbuddy.ai（intl）→ WorkBuddy/<版本> 正常，带 credits
//
// 曾经据此断言「dom 没有 credits 字段」，实为用错 UA 得出的错误结论。
func TestWBCatalogUAIsLineSpecific(t *testing.T) {
	// 国内必须带 CLI 身份，否则一个倍率都拿不到
	wantDom := "CLI/" + wbCatalogCLIVersion + " CodeBuddy/" + wbCatalogCLIVersion
	if got := wbCatalogUA("dom"); got != wantDom {
		t.Errorf("dom 目录 UA = %q，应为 %q（否则上游不下发 credits）", got, wantDom)
	}
	// 国际继续用 WorkBuddy/<版本>
	wantIntl := "WorkBuddy/" + wbCatalogVersion
	if got := wbCatalogUA("intl"); got != wantIntl {
		t.Errorf("intl 目录 UA = %q，应为 %q", got, wantIntl)
	}
	// 其它线路不读目录，但 UA 也不该是空串（防御性）
	if got := wbCatalogUA("qoder"); got == "" {
		t.Error("qoder 目录 UA 不该为空")
	}
}

func TestWBCatalogTTLIsDayish(t *testing.T) {
	// 上游上新是天级的。TTL 太短会让每次请求都打一次目录接口，纯属浪费配额。
	if wbCatalogTTL < time.Hour {
		t.Errorf("wbCatalogTTL = %v，太短", wbCatalogTTL)
	}
	// 顺带确认快照能序列化回自身（字段没漏 tag）。
	b, err := json.Marshal(&wbCatalog{Line: "intl", FetchedAt: time.Now(), Entries: []wbCatalogEntry{{ID: "a", Chat: true}}})
	if err != nil {
		t.Fatal(err)
	}
	var back wbCatalog
	if err := json.Unmarshal(b, &back); err != nil || back.Entries[0].ID != "a" {
		t.Fatalf("快照序列化往返失败：%v %v", err, string(b))
	}
}

// 显示名拼接：倍率 + 上下文。DSH 的解析器不读自定义字段，只读 name，
// 所以上下文必须并进名字才看得见（2026-10-15 用户报「选聚合网关模型显示英文 slug、
// 没有倍率」的根因之一）。
func TestHumanTokens(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want string
	}{{1000000, "1M"}, {2000000, "2M"}, {256000, "256K"}, {131072, "131K"}, {900, "900"}, {0, "0"}} {
		if got := humanTokens(tc.in); got != tc.want {
			t.Errorf("humanTokens(%d) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestRatioNamePrefersUpstreamCatalog(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	wbCatalogMu.Lock()
	wbCatalogCache = map[string]*wbCatalog{}
	wbCatalogMu.Unlock()
	t.Cleanup(func() {
		hubRoot = old
		wbCatalogMu.Lock()
		wbCatalogCache = map[string]*wbCatalog{}
		wbCatalogMu.Unlock()
	})

	// 目录里有一个手工表没有的模型：只有走目录才有倍率可标。
	cat, err := parseWBConfigBody(wbTestSample(t), "intl")
	if err != nil {
		t.Fatal(err)
	}
	wbCatalogPut(cat)

	name := ratioName("intl", "glm-5.3")
	if name == "" {
		t.Fatal("目录里有 glm-5.3（x0.79），显示名不该为空")
	}
	if !strings.Contains(name, "0.79x") || !strings.Contains(name, "上下文 1M") {
		t.Errorf("显示名应同时带倍率与上下文，得到 %q", name)
	}
	// 免费档要显示「免费」而不是「0x」。
	if got := ratioName("intl", "deepseek-v4.1-flash"); !strings.Contains(got, "免费") {
		t.Errorf("x0.00 应显示免费，得到 %q", got)
	}
	// 目录里没有、静态表也没有 → 返回空串，让调用方回退用 id（不硬编）。
	if got := ratioName("intl", "根本没这个模型"); got != "" {
		t.Errorf("两处都查不到时应返回空串，得到 %q", got)
	}
	// withCtx：上下文为 0 不该带出「上下文 0」。
	if got := withCtx("m", "0.5x", 0); got != "m · 0.5x" {
		t.Errorf("无上下文时不该拼这段：%q", got)
	}
}

// 2026-10-19 起 dom 也用上游目录：用对 UA 后国内能拿到 32/34 个倍率。
// 这条钉住范围——别再把 dom 踢出去，否则国内倍率又会退回手工表。
func TestWBCatalogAppliesToIntlAndDom(t *testing.T) {
	for _, line := range []string{"intl", "dom"} {
		if !wbCatalogApplies(line) {
			t.Fatalf("%s 必须用上游目录：倍率和上下文就在那儿", line)
		}
	}
	// 这些线路没有 /v3/config 目录接口，不该走
	for _, line := range []string{"qoder", "trae", "zcode", "oc"} {
		if wbCatalogApplies(line) {
			t.Errorf("%s 不该走 /v3/config 目录", line)
		}
	}
}

// dom 快照必须能被查到（对照旧行为：原先这里断言查不到）。
func TestWBDomCatalogLookupWorks(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	wbCatalogMu.Lock()
	wbCatalogCache = map[string]*wbCatalog{}
	wbCatalogMu.Unlock()
	t.Cleanup(func() {
		hubRoot = old
		wbCatalogMu.Lock()
		wbCatalogCache = map[string]*wbCatalog{}
		wbCatalogMu.Unlock()
	})
	cat, err := parseWBConfigBody(wbTestSample(t), "dom")
	if err != nil {
		t.Fatal(err)
	}
	wbCatalogPut(cat)
	if _, ok := wbCatalogLookup("dom", "deepseek-v4.1-flash"); !ok {
		t.Fatal("dom 快照应能被查到 —— 国内倍率全靠它")
	}
	if wbCatalogGet("dom") == nil {
		t.Fatal("wbCatalogGet 对 dom 应返回快照")
	}
}

// 国内上游返回的真实形状：只有 maxInputTokens、没有 contextWindow 对象。
// 解析必须回退到 maxInputTokens，否则上下文会全是 0。
func TestParseWBDomShapeWithoutContextWindowObject(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[
		{"id":"glm-5.3","name":"GLM-5.3","credits":"x0.79 credits",
		 "descriptionZh":"旗舰模型","maxInputTokens":1000000,"maxOutputTokens":64000,
		 "supportsImages":true,"supportsReasoning":true},
		{"id":"step-5-preview","name":"Step 5 Preview","credits":"x0.00 credits",
		 "maxInputTokens":1000000,"maxOutputTokens":64000}
	]}}`)
	cat, err := parseWBConfigBody(body, "dom")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 2 {
		t.Fatalf("应解析出 2 个模型，实际 %d", len(cat.Entries))
	}
	e, ok := cat.byID()["glm-5.3"]
	if !ok {
		t.Fatal("glm-5.3 未解析出来")
	}
	if e.Ratio != "0.79x" {
		t.Errorf("倍率 = %q，应为 0.79x", e.Ratio)
	}
	// 关键：没有 contextWindow 对象时，上下文必须回退到 maxInputTokens
	if e.Context != 1000000 {
		t.Errorf("上下文 = %d，应从 maxInputTokens 回退到 1000000", e.Context)
	}
	if e.Name != "GLM-5.3" {
		t.Errorf("名称 = %q", e.Name)
	}
	if e.Desc != "旗舰模型" {
		t.Errorf("中文描述 = %q", e.Desc)
	}
	// x0.00 → 免费
	if e2 := cat.byID()["step-5-preview"]; e2.Ratio != "免费" {
		t.Errorf("step-5-preview 倍率 = %q，应为免费", e2.Ratio)
	}
}
