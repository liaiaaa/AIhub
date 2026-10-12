package main

// Zcode（Z.ai）思考控制：客户端意图 → Z.ai wire 字段。
//
// Z.ai 的思考协议与 Qoder 完全不同（2026-09-26 官方文档核对）：
//   - thinking.type = "enabled" | "disabled"（GLM-4.x/5.2 可关；GLM-5.3 系
//     仅接受 enabled，发 disabled 会被拒）
//   - reasoning_effort = low | high | max（GLM-5.3 系深度档，默认 max）
//   - GLM-5.3 系「关闭思考」的官方替代：thinking enabled + effort 压到 low
//
// 因此映射规则：
//   意图 none  → 支持关闭的模型 thinking.type=disabled；5.3 系 → enabled + effort=low
//   意图档位   → thinking.type=enabled + reasoning_effort=钳制后的档
//   无意图     → 思考模型取该模型支持的最高档（2026-10-08 用户要求：客户端
//                没发表达档位就用最高档，不再用目录默认档）；efforts 为空/
//                未知回退目录默认档；非思考模型不动 body

import (
	"fmt"
	"strings"
)

// zcodeThinkingSpec 客户端思考意图的归一化结果（与 Qoder 同构）。
type zcodeThinkingSpec struct {
	Effort  string // none|low|medium|high|xhigh|max；空 = 未表达
	Enabled *bool  // enable_thinking / thinking.type=enabled；nil = 未表达
}

// zcodeResolveThinking 解析客户端意图（消息内标记由调用方先行处理）。
// 复用 Qoder 的归一化/换算辅助（qoderNormalizeEffort 等是通用规则）。
func zcodeResolveThinking(raw map[string]any, entry *zcodeModelEntry) zcodeThinkingSpec {
	spec := zcodeThinkingSpec{}
	explicit := false

	for _, k := range []string{"reasoning_effort", "effort"} {
		if s, ok := raw[k].(string); ok {
			if e := qoderNormalizeEffort(s); e != "" {
				spec.Effort = e
				explicit = true
				break
			}
		}
	}
	if th, ok := raw["thinking"].(map[string]any); ok {
		explicit = true
		switch strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", th["type"]))) {
		case "disabled", "off", "none":
			spec.Effort = "none"
		case "enabled":
			t := true
			spec.Enabled = &t
		}
	}
	if v, ok := raw["enable_thinking"].(bool); ok {
		explicit = true
		spec.Enabled = &v
	}
	// budget → 档位换算（Z.ai 无 budget 字段，仅作意图强度参考）
	if b, ok := qoderIntVal(raw["reasoning_budget_tokens"]); ok && b > 0 && spec.Effort == "" {
		spec.Effort = qoderEffortForBudget(b)
		explicit = true
	} else if b, ok := qoderIntVal(raw["thinking_budget"]); ok && b > 0 && spec.Effort == "" {
		spec.Effort = qoderEffortForBudget(b)
		explicit = true
	}

	// 显式关闭压倒一切
	if spec.Enabled != nil && !*spec.Enabled {
		spec.Effort = "none"
	}

	// 默认（2026-10-08 用户要求）：客户端没发档位就用该模型支持的最高档，
	// 与 Qoder 线路同语义（不是目录默认档）。Efforts 为空/未知 → 回退目录
	// 默认档 DefaultEffort；DefaultEffort 也为空 = 不发 effort（如 glm-5.2
	// 早期快照，Z.ai 默认仅 enabled 不带 effort）。
	if !explicit && spec.Effort == "" && entry != nil && entry.IsReasoning {
		spec.Effort = zcodeHighestEffort(entry)
	}

	// 档位不在支持列表时就近钳制（同距取更高档），绝不发非法值
	if spec.Effort != "" && spec.Effort != "none" && entry != nil && len(entry.Efforts) > 0 {
		ok := false
		for _, e := range entry.Efforts {
			if e == spec.Effort {
				ok = true
				break
			}
		}
		if !ok {
			spec.Effort = zcodeClampEffort(spec.Effort, entry)
		}
	}
	return spec
}

// zcodeHighestEffort 客户端未表达意图时该模型应使用的档位 = Efforts 里的最高档
// （2026-10-08：与 Qoder 线路同语义，默认最强，不用目录默认档）。
// 复用 Qoder 侧的规范序与比较（qoderEffortRank / qoderHighestEffort），不重复实现。
// 两条护栏：
//   - "none" 与未登记叫法（rank<0，如 "minimal"）不参与「最高档」判定，
//     否则 Efforts=["none"] 这类目录会把「未表达意图」翻成「关思考」；
//   - 无可比档（Efforts 空/全是上面这些）时回退目录默认档 DefaultEffort，
//     保持旧行为（DefaultEffort 也为空 → 不发 effort）。
func zcodeHighestEffort(entry *zcodeModelEntry) string {
	if entry == nil {
		return ""
	}
	cands := make([]string, 0, len(entry.Efforts))
	for _, e := range entry.Efforts {
		if e == "none" || qoderEffortRank(e) < 0 {
			continue
		}
		cands = append(cands, e)
	}
	return qoderHighestEffort(cands, entry.DefaultEffort)
}

// zcodeClampEffort 就近钳制：|距离|最小者；同距取更高档。
func zcodeClampEffort(effort string, entry *zcodeModelEntry) string {
	best := ""
	bestDist := 1 << 30
	for _, e := range entry.Efforts {
		d := qoderEffortRank(effort) - qoderEffortRank(e)
		if d < 0 {
			d = -d
		}
		if d < bestDist || (d == bestDist && qoderEffortRank(e) > qoderEffortRank(best)) {
			best, bestDist = e, d
		}
	}
	if best == "" {
		return entry.DefaultEffort
	}
	return best
}

// zcodeApplyThinking 把思考意图写进上游 body：
//   - body["thinking"]（保留客户端 thinking 对象里的其他字段如 clear_thinking）
//   - body["reasoning_effort"]（仅在发档位时）
//
// 并把已翻译/不属于 Z.ai 的顶层字段摘掉（防双重语义/上游噪音）。
// marked = 最后一条用户消息带 [no-thinking] 标记（调用方负责摘除标记本身）。
func zcodeApplyThinking(body map[string]any, entry *zcodeModelEntry, marked bool) {
	// 先读意图（此时 body["reasoning_effort"] 还是客户端原值），再摘除重写
	spec := zcodeThinkingSpec{Effort: "none"}
	if !marked {
		spec = zcodeResolveThinking(body, entry)
	}
	delete(body, "reasoning_effort")
	// OpenAI 专有字段，Z.ai 未文档化，摘掉防 400
	delete(body, "verbosity")
	delete(body, "reasoning_summary")

	thinking := map[string]any{}
	if th, ok := body["thinking"].(map[string]any); ok {
		for k, v := range th {
			thinking[k] = v
		}
	}

	// 未知模型（目录未收录）：不主动注入思考字段，客户端带了就原样保留
	if entry == nil {
		if spec.Effort != "none" {
			if spec.Effort != "" {
				body["reasoning_effort"] = spec.Effort
			}
			if len(thinking) > 0 {
				body["thinking"] = thinking
			}
		} else if len(thinking) > 0 {
			thinking["type"] = "disabled"
			body["thinking"] = thinking
		}
		for _, k := range []string{"effort", "enable_thinking", "thinking_budget", "reasoning_budget_tokens"} {
			delete(body, k)
		}
		return
	}

	supportsDisable := entry.SupportsDisable
	switch {
	case spec.Effort == "none":
		if supportsDisable {
			thinking["type"] = "disabled"
		} else {
			// 5.3 系不可关闭：官方迁移指引 = enabled + effort 压到 low
			thinking["type"] = "enabled"
			body["reasoning_effort"] = "low"
		}
	case spec.Effort != "":
		thinking["type"] = "enabled"
		body["reasoning_effort"] = spec.Effort
	case spec.Enabled != nil && *spec.Enabled:
		// 仅显式开思考（无档位意图）：enabled，不发档
		thinking["type"] = "enabled"
	default:
		// 无任何意图且算不出档位（Efforts 为空/未知且 DefaultEffort 也为空，
		// 如 glm-4.6 / glm-5.1 这类没登记档位的条目）：把 thinking.type 显式
		// 落成 enabled（Z.ai 默认即 enabled，显式化便于诊断），不发 effort
		if entry.IsReasoning {
			thinking["type"] = "enabled"
		}
	}
	if len(thinking) > 0 {
		body["thinking"] = thinking
	}
	for _, k := range []string{"effort", "enable_thinking", "thinking_budget", "reasoning_budget_tokens"} {
		delete(body, k)
	}
}

// zcodeThinkDiag 思考链路一行诊断。
func zcodeThinkDiag(raw map[string]any, marked bool, entry *zcodeModelEntry, body map[string]any) string {
	var b strings.Builder
	b.WriteString("意图{")
	first := true
	writeKV := func(k string, v any) {
		if !first {
			b.WriteString(" ")
		}
		first = false
		fmt.Fprintf(&b, "%s=%v", k, v)
	}
	for _, k := range []string{"reasoning_effort", "effort", "enable_thinking",
		"reasoning_budget_tokens", "thinking_budget"} {
		if v, ok := raw[k]; ok {
			writeKV(k, v)
		}
	}
	if th, ok := raw["thinking"].(map[string]any); ok {
		writeKV("thinking", th)
	}
	if marked {
		writeKV("[no-thinking]", true)
	}
	if first {
		b.WriteString("无显式字段")
	}
	b.WriteString("}")
	if entry == nil {
		b.WriteString(" 目录{未命中}")
	} else {
		fmt.Fprintf(&b, " 目录{efforts=%v default=%s disabled=%v}",
			entry.Efforts, entry.DefaultEffort, entry.SupportsDisable)
	}
	th, _ := body["thinking"].(map[string]any)
	fmt.Fprintf(&b, " → wire{thinking=%v reasoning_effort=%v}", th["type"], body["reasoning_effort"])
	return b.String()
}
