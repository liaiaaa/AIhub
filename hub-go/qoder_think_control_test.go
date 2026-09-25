package main

// 思考强度控制（真实上游参数）回归测试。
//
// 背景（2026-09-24 破解结论）：
//   - Qoder 网关真实思考开关在请求体 `parameters` 对象内（官方 worker runtime 的
//     A6e 组装函数 + proto/chat.proto 交叉证实）：
//     parameters.reasoning_effort = none|low|medium|high|xhigh|max
//     parameters.enable_thinking  = true|false
//     parameters.reasoning_budget_tokens = N（budget→effort 换算见 kZ：≤1024 low，
//     ≤8192 medium，≤24576 high，≤49152 xhigh，更大 max）
//     parameters.preserve_thinking = true|false
//   - 模型目录 thinking_config 决定每个模型支持的强度档位与默认档。
//   - 旧实现从未发送这些字段（只注入强制 <think> 格式的系统提示词），
//     既没有真思考又扭曲输出格式，是“一点不思考、干什么都干杂”的根因。
//
// 本文件锁定「客户端意图 → parameters 字段」的翻译行为。

import (
	"encoding/json"
	"strings"
	"testing"
)

// helperParam 取 body.parameters 中的一个字段。
func helperParam(t *testing.T, body map[string]any, key string) any {
	t.Helper()
	p, ok := body["parameters"].(map[string]any)
	if !ok {
		return nil
	}
	return p[key]
}

// helperQfEntry 返回内置目录里的 qfmodel 条目（efforts low/medium/xhigh，默认 medium）。
func helperQfEntry(t *testing.T) *qoderModelEntry {
	t.Helper()
	for _, m := range defaultQoderCatalog() {
		if m.Key == "qfmodel" {
			return &m
		}
	}
	t.Fatal("内置目录缺少 qfmodel")
	return nil
}

// 客户端显式 reasoning_effort 必须落到 parameters.reasoning_effort，并开启思考。
func TestApplyThinkingExplicitEffort(t *testing.T) {
	body := map[string]any{
		"model":            "qfmodel",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "xhigh",
	}
	qoderApplyThinking(body, helperQfEntry(t))
	if got := helperParam(t, body, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh", got)
	}
	if got := helperParam(t, body, "enable_thinking"); got != true {
		t.Errorf("parameters.enable_thinking = %v, want true", got)
	}
}

// thinking:{type:"disabled"} 必须关思考（effort=none + enable_thinking=false）。
func TestApplyThinkingDisabledByThinkingField(t *testing.T) {
	body := map[string]any{
		"model":    "qfmodel",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking": map[string]any{"type": "disabled"},
	}
	qoderApplyThinking(body, helperQfEntry(t))
	if got := helperParam(t, body, "reasoning_effort"); got != "none" {
		t.Errorf("parameters.reasoning_effort = %v, want none", got)
	}
	if got := helperParam(t, body, "enable_thinking"); got != false {
		t.Errorf("parameters.enable_thinking = %v, want false", got)
	}
}

// 无显式意图时：默认取该模型支持的最高档（qfmodel = xhigh），即默认最强思考。
func TestApplyThinkingDefaultsToHighestEffort(t *testing.T) {
	body := map[string]any{
		"model":    "qfmodel",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	qoderApplyThinking(body, helperQfEntry(t))
	if got := helperParam(t, body, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh（目录最高档）", got)
	}
}

// 请求了模型不支持的强度档：就近钳制到支持档（|距离|最小；同距取更高档），
// 绝不发非法值。qfmodel 支持 [low,medium,xhigh]：max→xhigh，high→xhigh。
func TestApplyThinkingUnsupportedEffortClampsToNearest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"max", "xhigh"},
		{"high", "xhigh"},
	}
	for _, c := range cases {
		body := map[string]any{
			"model":            "qfmodel",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": c.in,
		}
		qoderApplyThinking(body, helperQfEntry(t))
		if got := helperParam(t, body, "reasoning_effort"); got != c.want {
			t.Errorf("reasoning_effort=%q → %v, want %q（就近钳制）", c.in, got, c.want)
		}
	}
}

// 消息内 [no-thinking] 标记 = 本次关闭思考，且标记摘除不透传。
func TestApplyThinkingNoThinkMarker(t *testing.T) {
	body := map[string]any{
		"model": "qfmodel",
		"messages": []any{
			map[string]any{"role": "user", "content": "[no-thinking] 帮我写个函数"},
		},
	}
	qoderApplyThinking(body, helperQfEntry(t))
	if got := helperParam(t, body, "reasoning_effort"); got != "none" {
		t.Errorf("parameters.reasoning_effort = %v, want none", got)
	}
	if got := helperParam(t, body, "enable_thinking"); got != false {
		t.Errorf("parameters.enable_thinking = %v, want false", got)
	}
	msgs := body["messages"].([]any)
	content := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if content != "帮我写个函数" {
		t.Errorf("标记应被摘除，实际 content = %q", content)
	}
}

// thinking_budget → parameters.reasoning_budget_tokens，并按官方 kZ 换算强度档
// （30000 → xhigh）。
func TestApplyThinkingBudgetMapsToEffort(t *testing.T) {
	body := map[string]any{
		"model":           "qfmodel",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking_budget": 30000,
	}
	qoderApplyThinking(body, helperQfEntry(t))
	if got := helperParam(t, body, "reasoning_budget_tokens"); got != 30000 {
		t.Errorf("parameters.reasoning_budget_tokens = %v, want 30000", got)
	}
	if got := helperParam(t, body, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh（budget 换算）", got)
	}
}

// 别名归一：minimal→low、disabled/off→none（entry 传 nil 只测归一，不触发档位钳制）。
func TestApplyThinkingEffortAliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"minimal", "low"},
		{"Low", "low"},
		{"disabled", "none"},
		{"off", "none"},
		{"xhigh", "xhigh"},
		{"max", "max"},
	}
	for _, c := range cases {
		body := map[string]any{
			"model":            "qfmodel",
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": c.in,
		}
		qoderApplyThinking(body, nil)
		if got := helperParam(t, body, "reasoning_effort"); got != c.want {
			t.Errorf("reasoning_effort=%q → %v, want %q", c.in, got, c.want)
		}
	}
}

// gmodel 支持 [low,high,max]：请求 xhigh 时 high 与 max 同距，取更高档 max。
func TestApplyThinkingUnsupportedEffortFallsBackToDefault(t *testing.T) {
	entry := &qoderModelEntry{Key: "gmodel", Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"}
	body := map[string]any{
		"model":            "gmodel",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "xhigh", // gmodel 不支持 xhigh
	}
	qoderApplyThinking(body, entry)
	if got := helperParam(t, body, "reasoning_effort"); got != "max" {
		t.Errorf("parameters.reasoning_effort = %v, want max（就近钳制，同距取更高档）", got)
	}
}

// 可见思考提示词：默认注入**温和版**（要求 <think> 内思考、但明确保护输出格式），
// 严禁回归旧版刚性措辞（“严格按两部分组织/缺一不可”——那是“干什么都干杂”的元凶）。
func TestApplyThinkingInjectsGentlePrompt(t *testing.T) {
	body := map[string]any{
		"model": "qfmodel",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "帮我写个函数"},
		},
	}
	qoderApplyThinking(body, helperQfEntry(t))
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)
	content, _ := sys["content"].(string)
	if sys["role"] != "system" || !strings.Contains(content, "<think>") {
		t.Fatalf("应在 system 注入温和思考提示，实际 %v", sys)
	}
	if !strings.Contains(content, "必须先把完整的推理过程写在") {
		t.Errorf("提示词必须强制写 >tag（温和措辞实测不被遵守），实际 %q", content)
	}
	if !strings.Contains(content, "不要把思考内容混入其中") {
		t.Errorf("提示词必须保护输出格式（含“不要把思考内容混入其中”），实际 %q", content)
	}
	if strings.Contains(content, "缺一不可") || strings.Contains(content, "严格按以下两部分") {
		t.Errorf("禁止回归刚性措辞，实际 %q", content)
	}
}

// 带工具调用的请求同样注入思考提示（用户拍板：不管如何都开思考）。
// 措辞里“不要把思考内容混入其中”负责保护工具调用 JSON 不被思考污染。
func TestApplyThinkingInjectsPromptWithTools(t *testing.T) {
	body := map[string]any{
		"model": "qfmodel",
		"messages": []any{
			map[string]any{"role": "user", "content": "帮我改代码"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "edit"}}},
	}
	qoderApplyThinking(body, helperQfEntry(t))
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)
	content, _ := sys["content"].(string)
	if sys["role"] != "system" || !strings.Contains(content, "不要把思考内容混入其中") {
		t.Fatalf("带 tools 的请求也应注入思考提示，实际 %v", sys)
	}
	// 真实思考参数照常发送（默认档 = 该模型最高档 xhigh）
	if got := helperParam(t, body, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh", got)
	}
}

// 显式关闭思考时：不注入提示词、不动 messages（回归保护）。
func TestApplyThinkingDisabledTouchesNothing(t *testing.T) {
	body := map[string]any{
		"model":    "qfmodel",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking": map[string]any{"type": "disabled"},
	}
	before, _ := json.Marshal(body["messages"])
	qoderApplyThinking(body, helperQfEntry(t))
	after, _ := json.Marshal(body["messages"])
	if string(before) != string(after) {
		t.Errorf("关闭思考时不得改动 messages:\n before=%s\n after =%s", before, after)
	}
	if got := helperParam(t, body, "reasoning_effort"); got != "none" {
		t.Errorf("parameters.reasoning_effort = %v, want none", got)
	}
}

// 目录解析：官方 thinking_config → efforts / 默认档 / 是否支持关闭。
func TestParseQoderThinkingConfig(t *testing.T) {
	raw := []byte(`{"chat":[
		{"key":"qfmodel","display_name":"Qwen3.8-Flash","is_reasoning":true,
		 "thinking_config":{"disabled":{},"enabled":{"efforts":{"xhigh":{},"low":{},"medium":{"is_default":true}},"is_default":true}}},
		{"key":"gmodel","display_name":"GLM-5.3","is_reasoning":true,
		 "thinking_config":{"enabled":{"efforts":{"high":{},"low":{},"max":{"is_default":true}},"is_default":true}}},
		{"key":"mmodel","display_name":"MiniMax-M2.7","is_reasoning":false}
	]}`)
	entries, err := parseQoderModelList(raw)
	if err != nil {
		t.Fatalf("parseQoderModelList: %v", err)
	}
	byKey := map[string]qoderModelEntry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}

	qf := byKey["qfmodel"]
	if qf.DefaultEffort != "medium" {
		t.Errorf("qfmodel 默认档 = %q, want medium", qf.DefaultEffort)
	}
	if !equalStrSet(qf.Efforts, []string{"low", "medium", "xhigh"}) {
		t.Errorf("qfmodel efforts = %v, want [low medium xhigh]", qf.Efforts)
	}
	if !qf.SupportsDisabled {
		t.Error("qfmodel 有 disabled 档，应支持关闭")
	}

	g := byKey["gmodel"]
	if g.DefaultEffort != "max" {
		t.Errorf("gmodel 默认档 = %q, want max", g.DefaultEffort)
	}
	if g.SupportsDisabled {
		t.Error("gmodel 无 disabled 档，不应支持关闭")
	}

	m := byKey["mmodel"]
	if m.DefaultEffort != "" || len(m.Efforts) != 0 {
		t.Errorf("mmodel 无 thinking_config，应无强度信息，实际 efforts=%v default=%q", m.Efforts, m.DefaultEffort)
	}
}

func equalStrSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}
