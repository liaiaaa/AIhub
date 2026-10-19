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

func TestWBCatalogUAIsWorkbuddyForBothLines(t *testing.T) {
	// 接口按 UA 决定响应形状，大小写/前缀都敏感（workbuddy-desktop/5.7.6 实测被拒）。
	// 曾经以为国内要发 CodeBuddy/<版本>：那样发回来的 data 里根本没有 models 字段。
	// 两条线路都只认 WorkBuddy/<版本>，依据见 wbCatalogUA 的注释。
	for _, line := range []string{"intl", "dom", "qoder"} {
		if got := wbCatalogUA(line); got != "WorkBuddy/"+wbCatalogVersion {
			t.Errorf("%s 目录 UA = %q，应为 WorkBuddy/%s", line, got, wbCatalogVersion)
		}
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

// dom 不适用上游目录（见 wbCatalogApplies 里的实测依据）。这条钉住范围，
// 免得以后有人「统一一下」把 dom 也接上，用一份更旧还没有倍率的表覆盖手工表。
func TestWBCatalogAppliesOnlyToIntl(t *testing.T) {
	if !wbCatalogApplies("intl") {
		t.Fatal("intl 必须用上游目录：倍率和上下文就在那儿")
	}
	for _, line := range []string{"dom", "qoder", "trae", "zcode", "oc"} {
		if wbCatalogApplies(line) {
			t.Errorf("%s 不该走 intl 的上游目录", line)
		}
	}
	// 即使磁盘上残留着 dom 快照，查询也必须返回「查不到」。
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
	wbCatalogPut(cat) // 直接塞进缓存，绕过 applies 守卫，模拟残留
	if _, ok := wbCatalogLookup("dom", "deepseek-v4.1-flash"); ok {
		t.Fatal("dom 残留快照仍被查到 —— 守卫漏了")
	}
	if wbCatalogGet("dom") != nil {
		t.Fatal("wbCatalogGet 对 dom 必须返回 nil")
	}
	if n := wbCatalogFallbackNote("dom"); n == "" {
		t.Error("dom 该有独立的说明文案")
	}
}
