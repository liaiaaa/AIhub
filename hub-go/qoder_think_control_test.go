package main

// 思考强度控制（qodercli 信封 wire 形态）回归测试。
//
// 背景（2026-09-25 根因定论，修正 2026-09-24 的结论）：
//   - 思考开关在顶层 model_config.is_reasoning（chat_context.extra.modelConfig
//     同步），强度档在 parameters.reasoning_effort。
//   - 官方 wire 没有 parameters.enable_thinking（此前是我方臆测字段，已废弃）。
//   - 前提是完整 qodercli 信封：裸 OpenAI 体走网关兜底，思考配置整条被忽略
//     （「parameters 无可观测差异」的旧结论是无效对照）。
//
// 本文件锁定「客户端意图 → 信封 wire 字段」的翻译行为。
// 参考实现：dsh-provider-qoder（chat 请求仅带 max_tokens + reasoning_effort）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// helperEnvelope 用最小请求构建信封。
func helperEnvelope(raw map[string]any, entry *qoderModelEntry) map[string]any {
	msgs, _ := raw["messages"].([]any)
	if msgs == nil {
		msgs = []any{map[string]any{"role": "user", "content": "hi"}}
	}
	normMsgs := qoderNormalizeMessages(msgs)
	marked := qoderNoThinkMarked(normMsgs)
	if marked {
		qoderStripNoThinkMarker(normMsgs)
	}
	spec := qoderThinkingSpec{Effort: "none"}
	if !marked {
		spec = qoderResolveThinking(raw, normMsgs, entry)
	}
	return qoderEnvelope(normMsgs, raw["tools"], "qfmodel", "uid-1", spec, entry, raw)
}

// helperParam 取信封 parameters 中的一个字段。
func helperParam(t *testing.T, env map[string]any, key string) any {
	t.Helper()
	p, ok := env["parameters"].(map[string]any)
	if !ok {
		return nil
	}
	return p[key]
}

// helperModelConfig 取信封顶层 model_config。
func helperModelConfig(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	mc, ok := env["model_config"].(map[string]any)
	if !ok {
		t.Fatalf("信封缺少 model_config: %v", env)
	}
	return mc
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

// 信封基本形态：官方客户端字段必须齐——网关据此走正常（而非兜底）解析路径。
func TestEnvelopeWireShape(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"model":    "qfmodel",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, helperQfEntry(t))
	for _, k := range []string{"request_id", "request_set_id", "chat_record_id", "session_id",
		"stream", "chat_task", "is_reply", "source", "version", "session_type", "agent_id",
		"task_id", "messages", "parameters", "chat_context", "model_config", "business"} {
		if _, ok := env[k]; !ok {
			t.Errorf("信封缺少字段 %q", k)
		}
	}
	if env["agent_id"] != "agent_common" || env["session_type"] != "qodercli" || env["chat_task"] != "FREE_INPUT" {
		t.Errorf("agent_id/session_type/chat_task 不符: %v %v %v", env["agent_id"], env["session_type"], env["chat_task"])
	}
	// 客户端控制字段不得整体透传（避免双重语义/上游噪音）
	for _, k := range []string{"model", "stream_options", "reasoning_effort", "enable_thinking", "temperature"} {
		if _, ok := env[k]; ok {
			t.Errorf("信封不应透传客户端字段 %q", k)
		}
	}
}

// 客户端显式 reasoning_effort 必须落到 parameters.reasoning_effort + model_config.is_reasoning=true。
func TestEnvelopeExplicitEffort(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "xhigh",
	}, helperQfEntry(t))
	if got := helperParam(t, env, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh", got)
	}
	if got := helperModelConfig(t, env)["is_reasoning"]; got != true {
		t.Errorf("model_config.is_reasoning = %v, want true", got)
	}
	if got := helperParam(t, env, "enable_thinking"); got != nil {
		t.Errorf("enable_thinking 已废弃，不应发送，实际 %v", got)
	}
}

// thinking:{type:"disabled"} 必须关思考：不发 effort，is_reasoning=false。
func TestEnvelopeDisabledByThinkingField(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking": map[string]any{"type": "disabled"},
	}, helperQfEntry(t))
	if got := helperParam(t, env, "reasoning_effort"); got != nil {
		t.Errorf("关闭思考不应发 reasoning_effort，实际 %v", got)
	}
	if got := helperModelConfig(t, env)["is_reasoning"]; got != false {
		t.Errorf("model_config.is_reasoning = %v, want false", got)
	}
}

// 无显式意图时：默认取该模型支持的最高档（qfmodel = xhigh），即默认最强思考。
func TestEnvelopeDefaultsToHighestEffort(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, helperQfEntry(t))
	if got := helperParam(t, env, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh（目录最高档）", got)
	}
}

// 请求了模型不支持的强度档：就近钳制到支持档（|距离|最小；同距取更高档），
// 绝不发非法值。qfmodel 支持 [low,medium,xhigh]：max→xhigh，high→xhigh。
func TestEnvelopeUnsupportedEffortClampsToNearest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"max", "xhigh"},
		{"high", "xhigh"},
	}
	for _, c := range cases {
		env := helperEnvelope(map[string]any{
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": c.in,
		}, helperQfEntry(t))
		if got := helperParam(t, env, "reasoning_effort"); got != c.want {
			t.Errorf("reasoning_effort=%q → %v, want %q（就近钳制）", c.in, got, c.want)
		}
	}
}

// 消息内 [no-thinking] 标记 = 本次关闭思考，且标记摘除不透传。
func TestEnvelopeNoThinkMarker(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "[no-thinking] 帮我写个函数"},
		},
	}, helperQfEntry(t))
	if got := helperParam(t, env, "reasoning_effort"); got != nil {
		t.Errorf("关闭思考不应发 reasoning_effort，实际 %v", got)
	}
	if got := helperModelConfig(t, env)["is_reasoning"]; got != false {
		t.Errorf("model_config.is_reasoning = %v, want false", got)
	}
	msgs := env["messages"].([]any)
	content := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	if content != "帮我写个函数" {
		t.Errorf("标记应被摘除，实际 content = %q", content)
	}
}

// thinking_budget → 按官方 kZ 换算强度档（30000 → xhigh）。
// 预算 token 数本身不再透传（官方 wire 无此字段，插件同样只发 effort）。
func TestEnvelopeBudgetMapsToEffort(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking_budget": 30000,
	}, helperQfEntry(t))
	if got := helperParam(t, env, "reasoning_effort"); got != "xhigh" {
		t.Errorf("parameters.reasoning_effort = %v, want xhigh（budget 换算）", got)
	}
	if got := helperParam(t, env, "reasoning_budget_tokens"); got != nil {
		t.Errorf("reasoning_budget_tokens 不应发送（官方 wire 无此字段），实际 %v", got)
	}
}

// 别名归一：minimal→low、disabled/off→none（entry 传 nil 只测归一，不触发档位钳制）。
func TestEnvelopeEffortAliases(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"minimal", "low"},
		{"Low", "low"},
		{"disabled", nil},
		{"off", nil},
		{"xhigh", "xhigh"},
		{"max", "max"},
	}
	for _, c := range cases {
		env := helperEnvelope(map[string]any{
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": c.in,
		}, nil)
		if got := helperParam(t, env, "reasoning_effort"); got != c.want {
			t.Errorf("reasoning_effort=%q → %v, want %v", c.in, got, c.want)
		}
	}
}

// 目录未收录的模型 + 无显式意图：不发 effort，is_reasoning 回退 false（插件同款）；
// 但客户端显式 enable_thinking=true 时 is_reasoning=true。
func TestEnvelopeUnknownModel(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if got := helperModelConfig(t, env)["is_reasoning"]; got != false {
		t.Errorf("未知模型无意图 is_reasoning = %v, want false", got)
	}
	env = helperEnvelope(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"enable_thinking": true,
	}, nil)
	if got := helperModelConfig(t, env)["is_reasoning"]; got != true {
		t.Errorf("未知模型显式开思考 is_reasoning = %v, want true", got)
	}
}

// 无强度档的目录条目：is_reasoning 跟随目录（mmodel=false，auto=true）。
func TestEnvelopeIsReasoningFromCatalog(t *testing.T) {
	find := func(key string) *qoderModelEntry {
		for _, m := range defaultQoderCatalog() {
			if m.Key == key {
				return &m
			}
		}
		return nil
	}
	env := helperEnvelope(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, find("mmodel"))
	if got := helperModelConfig(t, env)["is_reasoning"]; got != false {
		t.Errorf("mmodel is_reasoning = %v, want false", got)
	}
	env = helperEnvelope(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, find("auto"))
	if got := helperModelConfig(t, env)["is_reasoning"]; got != true {
		t.Errorf("auto is_reasoning = %v, want true", got)
	}
}

// gmodel 支持 [low,high,max]：请求 xhigh 时 high 与 max 同距，取更高档 max。
func TestEnvelopeUnsupportedEffortTieTakesHigher(t *testing.T) {
	entry := &qoderModelEntry{Key: "gmodel", Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"}
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	spec := qoderResolveThinking(map[string]any{"reasoning_effort": "xhigh"}, msgs, entry)
	env := qoderEnvelope(qoderNormalizeMessages(msgs), nil, "gmodel", "uid-1", spec, entry, map[string]any{})
	if got := helperParam(t, env, "reasoning_effort"); got != "max" {
		t.Errorf("parameters.reasoning_effort = %v, want max（就近钳制，同距取更高档）", got)
	}
}

// max_tokens：客户端值钳制到 [1,32768]，缺省 32768，并同步 model_config.max_output_tokens。
func TestEnvelopeMaxTokens(t *testing.T) {
	cases := []struct {
		raw  any
		want int
	}{
		{nil, 32768},
		{float64(1024), 1024},
		{float64(999999), 32768},
	}
	for _, c := range cases {
		raw := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		if c.raw != nil {
			raw["max_tokens"] = c.raw
		}
		env := helperEnvelope(raw, nil)
		if got := helperParam(t, env, "max_tokens"); got != c.want {
			t.Errorf("max_tokens=%v → %v, want %v", c.raw, got, c.want)
		}
		if got := helperModelConfig(t, env)["max_output_tokens"]; got != c.want {
			t.Errorf("model_config.max_output_tokens = %v, want %v", got, c.want)
		}
	}
}

// 可见思考提示词：默认关闭（信封对齐后原生推理是唯一可靠杠杆）；
// QODER_THINK_PROMPT=on 显式开启时注入温和版，禁止回归刚性措辞。
func TestEnvelopePromptInjection(t *testing.T) {
	raw := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "帮我写个函数"},
		},
	}
	msgs := qoderNormalizeMessages(raw["messages"].([]any))
	spec := qoderResolveThinking(raw, msgs, helperQfEntry(t))
	env := qoderEnvelope(msgs, nil, "qfmodel", "uid-1", spec, helperQfEntry(t), raw)
	// 默认关
	sys := env["messages"].([]any)[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("不应新增注入消息，实际首条 %v", sys)
	}
	if content, _ := sys["content"].(string); content != "You are helpful." {
		t.Errorf("默认不注入提示词，实际 system = %q", content)
	}
	// 开启后注入
	t.Setenv("QODER_THINK_PROMPT", "on")
	env = qoderEnvelope(msgs, nil, "qfmodel", "uid-1", spec, helperQfEntry(t), raw)
	qoderInjectPrompt(env, qoderThinkingPrompt)
	sys = env["messages"].([]any)[0].(map[string]any)
	content, _ := sys["content"].(string)
	if !strings.Contains(content, "<think>") || !strings.Contains(content, "不要把思考内容混入其中") {
		t.Errorf("开启后应注入温和提示词，实际 %q", content)
	}
	if strings.Contains(content, "缺一不可") || strings.Contains(content, "严格按以下两部分") {
		t.Errorf("禁止回归刚性措辞，实际 %q", content)
	}
}

// 多轮对话中客户端回传的 assistant.reasoning_content 原样保留
// （等价 dsh-provider-qoder 的 preserveThinking=true）。
func TestEnvelopePreservesAssistantReasoning(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "第一问"},
		map[string]any{"role": "assistant", "content": "答案一", "reasoning_content": "第一轮思考"},
		map[string]any{"role": "user", "content": "继续"},
	}
	env := qoderEnvelope(msgs, nil, "qfmodel", "uid-1", qoderThinkingSpec{Effort: "xhigh"}, helperQfEntry(t), map[string]any{})
	wireMsgs := env["messages"].([]any)
	asst := wireMsgs[1].(map[string]any)
	if got, _ := asst["reasoning_content"].(string); got != "第一轮思考" {
		t.Errorf("assistant.reasoning_content 应保留, 实际 %q", got)
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

// 信封必须可序列化为合法 JSON（wire 侧基本保障）。
func TestEnvelopeMarshal(t *testing.T) {
	env := helperEnvelope(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, helperQfEntry(t))
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}
