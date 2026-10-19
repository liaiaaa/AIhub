package main

// 2026-10-15 改造的回归锁定：
//   1. 死 provider 段清理（cc-switch 时代指向 :15721 的段，接管时删掉）；
//   2. /v1/models 富信息形态（DSH 等客户端读中文显示名 + 倍率 + 上下文）；
//   3. gpt-* 官方模型不进目录但透传路由保留。

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- 1. 死段清理 ----------

func TestDropStaleRouterSections(t *testing.T) {
	in := []string{
		`model_provider = "custom"`,
		`model = "deepseek-v4.1-flash"`,
		``,
		`[model_providers.custom]`,
		`name = "custom"`,
		`base_url = "http://127.0.0.1:15721/v1"`,
		`wire_api = "responses"`,
		`experimental_bearer_token = "PROXY_MANAGED"`,
		``,
		`[model_providers.aihub]`,
		`name = "ChatGPT"`,
		`base_url = "http://127.0.0.1:8790/v1"`,
	}
	got := strings.Join(dropStaleRouterSections(in), "\n")

	if strings.Contains(got, "15721") {
		t.Fatalf("指向 :15721 的死段没被删掉：\n%s", got)
	}
	// aihub 段（:8790）必须原样保留
	for _, want := range []string{
		`[model_providers.aihub]`,
		`base_url = "http://127.0.0.1:8790/v1"`,
		`model_provider = "custom"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("误删了不该删的内容 %q：\n%s", want, got)
		}
	}
}

// 非 15721 的 provider 段不能被误删（比如用户自己配的官方段）。
func TestDropStaleRouterSectionsKeepsOthers(t *testing.T) {
	in := []string{
		`[model_providers.openai]`,
		`base_url = "https://api.openai.com/v1"`,
		``,
		`[model_providers.mine]`,
		`base_url = "http://127.0.0.1:9999/v1"`,
	}
	got := strings.Join(dropStaleRouterSections(in), "\n")
	for _, want := range []string{"[model_providers.openai]", "[model_providers.mine]", "9999"} {
		if !strings.Contains(got, want) {
			t.Fatalf("误删了 %q：\n%s", want, got)
		}
	}
}

// ---------- 2. 富信息 /v1/models ----------

func TestAggOpenAIModelsRichFields(t *testing.T) {
	h := &Hub{}
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{
		{Line: "intl", Slug: "intl.deepseek-v4.1-flash", Backend: "deepseek-v4.1-flash",
			Display: "DeepSeek 4.1 Flash（国际）"},
		{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash",
			Display: "Qwen3.8 Flash（Qoder）"},
	}
	aggMu.Unlock()
	defer func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() }()

	resp := h.aggOpenAIModels()
	data, _ := resp["data"].([]map[string]any)
	if len(data) != 2 {
		t.Fatalf("应有 2 个模型，实际 %d", len(data))
	}

	byID := map[string]map[string]any{}
	for _, e := range data {
		id, _ := e["id"].(string)
		byID[id] = e
	}

	// id 必须仍是聚合 slug（请求侧标识，不能改）
	if _, ok := byID["intl.deepseek-v4.1-flash"]; !ok {
		t.Fatalf("id 应为聚合 slug，实际 %v", data)
	}
	// name 必须是中文显示名 + 倍率，不能是英文 slug 打头
	ds := byID["intl.deepseek-v4.1-flash"]
	name, _ := ds["name"].(string)
	if !strings.Contains(name, "国际") {
		t.Fatalf("显示名缺线路归属：%q", name)
	}
	if !strings.Contains(name, "免费") {
		t.Fatalf("显示名缺倍率（国际 deepseek-v4.1-flash 应为免费）：%q", name)
	}
	// intl.deepseek-v4.1-flash 有实测上下文窗口 1M，应输出上下文字段
	if cw := ds["contextWindow"]; cw == nil {
		t.Fatalf("应有 contextWindow，实际缺失")
	} else if n, ok := cw.(int); !ok || n != 1048576 {
		t.Fatalf("contextWindow 应为 1048576，实际 %v", cw)
	}
	// qoder 的 Qwen3.8-Flash 倍率来自官方目录/静态表，应有倍率
	qw := byID["qoder.Qwen3.8-Flash"]
	qn, _ := qw["name"].(string)
	if !strings.Contains(qn, "0.1x") {
		t.Fatalf("qoder 显示名应含 0.1x，实际 %q", qn)
	}
}

// gpt-* 不进目录（用户要求目录只放 hub-go 线路模型）。
func TestAggCodexModelsHidesOfficial(t *testing.T) {
	h := &Hub{}
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.glm-5.3-flash", Backend: "glm-5.3-flash", Display: "GLM 5.3 Flash（国内）"}}
	aggMu.Unlock()
	defer func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() }()

	b, _ := json.Marshal(h.aggCodexModels())
	got := string(b)
	if strings.Contains(got, "gpt-") {
		t.Fatalf("Codex 目录里不该出现官方模型：%s", got)
	}
	if !strings.Contains(got, "dom.glm-5.3-flash") {
		t.Fatalf("白名单模型不见了：%s", got)
	}
}

// 透传路由必须保留：目录里没有，但 resolveAggModel 仍认 gpt-*。
func TestOfficialPassthroughStillResolves(t *testing.T) {
	am, ok := resolveAggModel("gpt-5.6-luna")
	if !ok {
		t.Fatal("gpt-* 透传路由被误删")
	}
	if am.Line != "official" {
		t.Fatalf("gpt-* 应归 official 线路，实际 %q", am.Line)
	}
}

// ---------- 友好显示名 ----------

func TestAggDisplayNamePretty(t *testing.T) {
	cases := []struct{ line, backend, want string }{
		{"intl", "kimi-k3", "Kimi K3（国际）"},
		{"intl", "gpt-6-luna", "GPT 6 Luna（国际）"},
		{"dom", "glm-5.3-flash", "GLM 5.3 Flash（国内）"},
		{"qoder", "Qwen3.8-Flash", "Qwen3.8 Flash（Qoder）"},
		// 未列入的回落 backend 原值
		{"intl", "some-unknown-model", "some-unknown-model（国际）"},
	}
	for _, c := range cases {
		if got := aggDisplayName(c.line, c.backend); got != c.want {
			t.Errorf("aggDisplayName(%q,%q) = %q, want %q", c.line, c.backend, got, c.want)
		}
	}
}

// 存量小写英文 id 的 display 应被规范化；用户手改的应保留。
func TestAggNormalizeDisplayLocked(t *testing.T) {
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{
		{Line: "intl", Slug: "intl.kimi-k3", Backend: "kimi-k3", Display: "kimi-k3（国际）"},
		{Line: "dom", Slug: "dom.glm-5.3-flash", Backend: "glm-5.3-flash", Display: "我自己改的名字"},
	}
	aggNormalizeDisplayLocked()
	got := map[string]string{aggList[0].Slug: aggList[0].Display, aggList[1].Slug: aggList[1].Display}
	aggList = saved
	aggMu.Unlock()

	if got["intl.kimi-k3"] != "Kimi K3（国际）" {
		t.Errorf("存量英文 id 未规范化：%q", got["intl.kimi-k3"])
	}
	if got["dom.glm-5.3-flash"] != "我自己改的名字" {
		t.Errorf("用户手改的显示名被覆盖了：%q", got["dom.glm-5.3-flash"])
	}
}
