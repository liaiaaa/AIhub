package main

// Zcode（Z.ai）思考控制回归测试。
//
// Z.ai 思考协议（2026-09-26 官方文档核对）：
//   - thinking.type = enabled|disabled（GLM-5.3 系仅接受 enabled，发 disabled 被拒）
//   - reasoning_effort = low|high|max（GLM-5.3 系深度档，默认 max）
//   - 5.3 系「关闭」的官方替代：enabled + effort 压到 low
//
// 本文件锁定「客户端意图 → Z.ai wire 字段」的翻译行为。

import (
	"encoding/json"
	"strings"
	"testing"
)

// helperZcodeEntry 按真实 key 取内置目录条目。
func helperZcodeEntry(t *testing.T, key string) *zcodeModelEntry {
	t.Helper()
	if e := zcodeEntryFor(zcodeDefaultCatalog(), key); e != nil {
		return e
	}
	t.Fatalf("内置目录缺少 %q", key)
	return nil
}

// GLM-5.3 无显式意图：默认最强档 max + thinking enabled。
func TestZcodeDefaultsToMaxEffort(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), false)
	th := body["thinking"].(map[string]any)
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if body["reasoning_effort"] != "max" {
		t.Errorf("reasoning_effort = %v, want max（Efforts 最高档）", body["reasoning_effort"])
	}
}

// 2026-10-08 用户要求（原话：「如果我没发送思考等级那就默认最高就行」）：
// 客户端未表达任何思考意图时，用该模型 Efforts 里的最高档，而不是目录默认档。
func TestZcodeNoIntentUsesHighestEffortNotCatalogDefault(t *testing.T) {
	cases := []struct {
		name  string
		entry *zcodeModelEntry
		want  string // 期望注入的 reasoning_effort；"" = 不发档位
	}{
		{"默认medium_四档取max",
			&zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
				Efforts: []string{"low", "medium", "high", "max"}, DefaultEffort: "medium"}, "max"},
		{"默认high_两档取max",
			&zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
				Efforts: []string{"high", "max"}, DefaultEffort: "high"}, "max"},
		{"目录里档位乱序",
			&zcodeModelEntry{Key: "m", IsReasoning: true,
				Efforts: []string{"max", "low", "high"}, DefaultEffort: "low"}, "max"},
		{"无max档时取xhigh",
			&zcodeModelEntry{Key: "m", IsReasoning: true,
				Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium"}, "xhigh"},
		{"档位含none不误判为关思考",
			&zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
				Efforts: []string{"none", "high", "max"}, DefaultEffort: ""}, "max"},
		{"档位含未登记叫法",
			&zcodeModelEntry{Key: "m", IsReasoning: true,
				Efforts: []string{"minimal", "low"}, DefaultEffort: ""}, "low"},
		{"无档位信息回退目录默认档",
			&zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
				Efforts: nil, DefaultEffort: "medium"}, "medium"},
		{"无档位无默认档不发",
			&zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
				Efforts: nil, DefaultEffort: ""}, ""},
		{"仅none档不发（不关思考）",
			&zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
				Efforts: []string{"none"}, DefaultEffort: ""}, ""},
		{"非思考模型不注入",
			&zcodeModelEntry{Key: "m", IsReasoning: false,
				Efforts: []string{"low", "max"}, DefaultEffort: ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := zcodeResolveThinking(map[string]any{}, c.entry)
			if spec.Effort != c.want {
				t.Errorf("resolve: Effort = %q, want %q", spec.Effort, c.want)
			}
			body := map[string]any{"messages": []any{}}
			zcodeApplyThinking(body, c.entry, false)
			got, exists := body["reasoning_effort"]
			if c.want == "" {
				if exists {
					t.Errorf("wire: 不应注入 reasoning_effort, 实际 %v", got)
				}
				return
			}
			if got != c.want {
				t.Errorf("wire: reasoning_effort = %v, want %q", got, c.want)
			}
			th, ok := body["thinking"].(map[string]any)
			if !ok || th["type"] != "enabled" {
				t.Errorf("wire: thinking = %v, want type=enabled", body["thinking"])
			}
		})
	}
}

// 显式档位优先于「默认最高」：客户端发 low 就保持 low，绝不被升档。
func TestZcodeExplicitLowNotUpgraded(t *testing.T) {
	entry := &zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
		Efforts: []string{"low", "medium", "high", "max"}, DefaultEffort: "medium"}
	for _, raw := range []map[string]any{
		{"reasoning_effort": "low"},
		{"effort": "low"},
		{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "low"},
	} {
		body := map[string]any{"messages": []any{}}
		for k, v := range raw {
			body[k] = v
		}
		zcodeApplyThinking(body, entry, false)
		if body["reasoning_effort"] != "low" {
			t.Errorf("raw=%v: reasoning_effort = %v, want low", raw, body["reasoning_effort"])
		}
		if body["thinking"].(map[string]any)["type"] != "enabled" {
			t.Errorf("raw=%v: thinking 应保持 enabled", raw)
		}
	}
}

// 显式关闭的优先级不受「默认最高」影响：可关→disabled 且不effort；不可关→low。
func TestZcodeExplicitDisableStillWins(t *testing.T) {
	disableable := &zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: true,
		Efforts: []string{"low", "high", "max"}, DefaultEffort: "medium"}
	body := map[string]any{"messages": []any{}, "enable_thinking": false}
	zcodeApplyThinking(body, disableable, false)
	if body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Errorf("可关模型应 thinking.type=disabled, 实际 %v", body["thinking"])
	}
	if _, exists := body["reasoning_effort"]; exists {
		t.Errorf("关闭时不应发 reasoning_effort, 实际 %v", body["reasoning_effort"])
	}

	fixed := &zcodeModelEntry{Key: "m", IsReasoning: true, SupportsDisable: false,
		Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"}
	body = map[string]any{"messages": []any{}, "reasoning_effort": "none"}
	zcodeApplyThinking(body, fixed, false)
	if body["thinking"].(map[string]any)["type"] != "enabled" {
		t.Errorf("不可关模型应 enabled")
	}
	if body["reasoning_effort"] != "low" {
		t.Errorf("不可关模型关闭意图应压到 low, 实际 %v", body["reasoning_effort"])
	}
}

// 未知模型（entry=nil）：即便启用「默认最高」也不注入任何思考字段。
func TestZcodeNoIntentUnknownModelNoInjection(t *testing.T) {
	spec := zcodeResolveThinking(map[string]any{}, nil)
	if spec.Effort != "" {
		t.Errorf("未知模型不应有默认档, 实际 %q", spec.Effort)
	}
	body := map[string]any{"messages": []any{}}
	zcodeApplyThinking(body, nil, false)
	if _, exists := body["thinking"]; exists {
		t.Errorf("未知模型无意图不应注入 thinking")
	}
	if _, exists := body["reasoning_effort"]; exists {
		t.Errorf("未知模型无意图不应注入 reasoning_effort")
	}
}

// GLM-5.3 显式 low：原样发送。
func TestZcodeExplicitEffort(t *testing.T) {
	body := map[string]any{"messages": []any{}, "reasoning_effort": "high"}
	zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), false)
	if body["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
	if body["thinking"].(map[string]any)["type"] != "enabled" {
		t.Errorf("思考应保持开启")
	}
}

// GLM-5.3 请求关闭思考：不可关闭 → enabled + effort 压到 low（官方迁移指引）。
func TestZcodeDisableClampsToLow(t *testing.T) {
	for _, raw := range []map[string]any{
		{"messages": []any{}, "reasoning_effort": "none"},
		{"messages": []any{}, "thinking": map[string]any{"type": "disabled"}},
		{"messages": []any{}, "enable_thinking": false},
	} {
		body := map[string]any{"messages": []any{}}
		for k, v := range raw {
			if k != "messages" {
				body[k] = v
			}
		}
		zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), false)
		th := body["thinking"].(map[string]any)
		if th["type"] != "enabled" {
			t.Errorf("raw=%v: 5.3 系不可关闭, thinking.type = %v, want enabled", raw, th["type"])
		}
		if body["reasoning_effort"] != "low" {
			t.Errorf("raw=%v: reasoning_effort = %v, want low（官方迁移指引）", raw, body["reasoning_effort"])
		}
	}
}

// [no-thinking] 消息内标记：glm-5.3 → low+enabled；标记本身不透传。
func TestZcodeNoThinkMarker(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "[no-thinking] 快回答"}}
	marked := qoderNoThinkMarked(msgs)
	if !marked {
		t.Fatal("应识别 [no-thinking] 标记")
	}
	qoderStripNoThinkMarker(msgs)
	body := map[string]any{"messages": msgs}
	zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), marked)
	if body["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low", body["reasoning_effort"])
	}
	content := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(content, "[no-thinking]") {
		t.Errorf("标记应被摘除, 实际 %q", content)
	}
}

// 档位钳制：glm-5.3 支持 [low,high,max]；medium→high（同距取更高档），xhigh→max。
func TestZcodeEffortClamp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"medium", "high"},
		{"xhigh", "max"},
		{"minimal", "low"},
	}
	for _, c := range cases {
		body := map[string]any{"messages": []any{}, "reasoning_effort": c.in}
		zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), false)
		if body["reasoning_effort"] != c.want {
			t.Errorf("reasoning_effort=%q → %v, want %q", c.in, body["reasoning_effort"], c.want)
		}
	}
}

// GLM-5.2：思考可关闭；关闭时不发 reasoning_effort；无意图时取 Efforts 最高档 max。
func TestZcodeGlm52Toggle(t *testing.T) {
	entry := helperZcodeEntry(t, "glm-5.2")
	body := map[string]any{"messages": []any{}, "reasoning_effort": "none"}
	zcodeApplyThinking(body, entry, false)
	if body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Errorf("5.2 应支持关闭, thinking.type = %v", body["thinking"].(map[string]any)["type"])
	}
	if body["reasoning_effort"] != nil {
		t.Errorf("5.2 关闭时不应发 reasoning_effort, 实际 %v", body["reasoning_effort"])
	}
	body = map[string]any{"messages": []any{}}
	zcodeApplyThinking(body, entry, false)
	if body["thinking"].(map[string]any)["type"] != "enabled" {
		t.Errorf("5.2 默认 enabled, 实际 %v", body["thinking"].(map[string]any)["type"])
	}
	// 2026-10-08：5.2 的 Efforts=[high max]、DefaultEffort="" —— 未指定档位时
	// 由「目录默认档（不发）」改为「最高档」，故现在应发 max。
	if body["reasoning_effort"] != "max" {
		t.Errorf("5.2 无意图应发最高档 max, 实际 %v", body["reasoning_effort"])
	}
}

// 客户端 thinking 对象里的其他字段（clear_thinking）必须保留。
func TestZcodePreservesThinkingExtraFields(t *testing.T) {
	body := map[string]any{
		"messages": []any{},
		"thinking": map[string]any{"type": "enabled", "clear_thinking": false},
	}
	zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3-flash"), false)
	th := body["thinking"].(map[string]any)
	if th["clear_thinking"] != false {
		t.Errorf("clear_thinking 应保留, 实际 %v", th["clear_thinking"])
	}
	if th["type"] != "enabled" {
		t.Errorf("type = %v, want enabled", th["type"])
	}
}

// OpenAI 专有字段（verbosity/reasoning_summary）与翻译后的意图字段必须摘掉。
func TestZcodeStripsForeignFields(t *testing.T) {
	body := map[string]any{
		"messages":          []any{},
		"verbosity":         "low",
		"reasoning_summary": "auto",
		"thinking_budget":   1024,
		"enable_thinking":   true,
	}
	zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), false)
	for _, k := range []string{"verbosity", "reasoning_summary", "thinking_budget", "enable_thinking", "effort"} {
		if _, exists := body[k]; exists {
			t.Errorf("字段 %q 不应透传给 Z.ai", k)
		}
	}
	// budget 1024 → low 档换算
	if body["reasoning_effort"] != "low" {
		t.Errorf("budget 换算 effort = %v, want low", body["reasoning_effort"])
	}
}

// 目录未收录的模型：不主动注入思考字段；无意图时 body 基本原样。
func TestZcodeUnknownModelPassthrough(t *testing.T) {
	body := map[string]any{"messages": []any{}, "temperature": float64(1)}
	zcodeApplyThinking(body, nil, false)
	if _, exists := body["thinking"]; exists {
		t.Errorf("未知模型无意图不应注入 thinking")
	}
	if _, exists := body["reasoning_effort"]; exists {
		t.Errorf("未知模型无意图不应注入 reasoning_effort")
	}
	if body["temperature"] != float64(1) {
		t.Errorf("无关字段应保留")
	}
}

// 别名表：显示名/大小写变体都能命中真实 key。
func TestZcodeAliases(t *testing.T) {
	catalog := zcodeDefaultCatalog()
	aliases := buildZcodeAliases(catalog)
	cases := map[string]string{
		"GLM-5.3":       "glm-5.3",
		"glm-5.3":       "glm-5.3",
		"GLM-5.3-Flash": "glm-5.3-flash",
		"glm5.3":        "glm-5.3",
		"GLM-5.2":       "glm-5.2",
		"default":       "glm-5.3",
	}
	for name, want := range cases {
		if got := aliases[qoderNormalizeAlias(name)]; got != want {
			t.Errorf("别名 %q → %q, want %q", name, got, want)
		}
	}
}

// 信封 JSON 序列化（wire 基本保障）。
func TestZcodeBodyMarshal(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	zcodeApplyThinking(body, helperZcodeEntry(t, "glm-5.3"), false)
	if _, err := json.Marshal(body); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
