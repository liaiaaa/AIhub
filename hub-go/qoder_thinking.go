package main

// 思考控制：effort 解析、消息内标记、诊断
// 由 proxy_qoder.go 拆分而来；2026-09-25 按官方 qodercli 信封重构。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ---------- 思考控制 ----------
//
// 背景（2026-09-25 根因定论，修正 2026-09-24 的结论）：
//  1. 思考开关在**顶层 model_config.is_reasoning**（chat_context.extra.
//     modelConfig.is_reasoning 同步），强度档在 parameters.reasoning_effort
//     （none|low|medium|high|xhigh|max）。官方 wire **没有** parameters.
//     enable_thinking——那是我方臆测字段，怎么发都无效。
//  2. 前提是请求体必须是完整 qodercli 信封（qoder_envelope.go）。此前裸 OpenAI
//     体走网关兜底路径，parameters 各形态自然「均无可观测差异」——无效对照。
//  3. 响应侧：原生 delta.reasoning_content 会流回；模型偶尔内联的
//     <think>/<thinking> 标签由流分流器转写（qoder_stream.go）。
//  4. 每个模型支持的强度档来自官方目录 thinking_config.efforts（/algo/api/v2/
//     model/list 拉取）。官方客户端每个请求都发送 parameters.reasoning_effort。
//
// 默认策略（2026-09-24 用户选择，保留）：无显式意图时取该模型支持的最高档。

const (
	// 消息内关闭标记（对任何客户端都可用的按次开关）。
	qoderNoThinkMarker    = "[no-thinking]"
	qoderNoThinkMarkerAlt = "[nothink]"
)

// qoderEffortOrder 强度规范序（与官方 runtime 一致）。
var qoderEffortOrder = []string{"none", "low", "medium", "high", "xhigh", "max"}

// qoderNormalizeEffort 把各种叫法归一为官方强度档；无法识别返回空串。
func qoderNormalizeEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "disabled", "off", "disable":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium", "moderate":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "ultra":
		return "xhigh"
	case "max", "maximum":
		return "max"
	}
	return ""
}

// qoderEffortForBudget 按官方 kZ 换算：预算 token 数 → 强度档。
func qoderEffortForBudget(n int) string {
	switch {
	case n <= 0:
		return "none"
	case n <= 1024:
		return "low"
	case n <= 8192:
		return "medium"
	case n <= 24576:
		return "high"
	case n <= 49152:
		return "xhigh"
	default:
		return "max"
	}
}

// qoderIntVal 从 json.Number / float64 / int 等取整数值。
func qoderIntVal(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return int(i), true
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// qoderThinkingSpec 客户端思考意图的归一化结果。
type qoderThinkingSpec struct {
	Effort   string // none|low|medium|high|xhigh|max；空 = 未表达
	Enabled  *bool  // enable_thinking / thinking.type=enabled；nil = 未表达
	Budget   int    // reasoning_budget_tokens / thinking_budget；0 = 未表达
	Preserve *bool  // preserve_thinking；nil = 未表达
}

// qoderResolveThinking 解析客户端意图（优先级：消息内标记 > 请求体显式字段 >
// 目录最高档），并做两步规范化：
//   - budget → 强度档换算（未显式给档位时）
//   - 档位不在该模型支持列表时就近钳制（绝不发非法值）
func qoderResolveThinking(raw map[string]any, msgs []any, entry *qoderModelEntry) qoderThinkingSpec {
	spec := qoderThinkingSpec{}
	explicit := false

	// 1) 显式强度字段（OpenAI 风格 reasoning_effort / 简写 effort）
	for _, k := range []string{"reasoning_effort", "effort"} {
		if s, ok := raw[k].(string); ok {
			if e := qoderNormalizeEffort(s); e != "" {
				spec.Effort = e
				explicit = true
				break
			}
		}
	}

	// 2) thinking:{type,budget_tokens}（DeepSeek 风格）
	if th, ok := raw["thinking"].(map[string]any); ok {
		explicit = true
		switch strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", th["type"]))) {
		case "disabled", "off", "none":
			spec.Effort = "none"
		case "enabled":
			t := true
			spec.Enabled = &t
		}
		if b, ok := qoderIntVal(th["budget_tokens"]); ok {
			spec.Budget = b
		}
	}

	// 3) enable_thinking / budget / preserve
	if v, ok := raw["enable_thinking"].(bool); ok {
		explicit = true
		spec.Enabled = &v
	}
	if b, ok := qoderIntVal(raw["reasoning_budget_tokens"]); ok {
		explicit = true
		spec.Budget = b
	} else if b, ok := qoderIntVal(raw["thinking_budget"]); ok {
		explicit = true
		spec.Budget = b
	}
	if v, ok := raw["preserve_thinking"].(bool); ok {
		spec.Preserve = &v
	}

	// budget → 档位换算（仅在未显式指定档位时）
	if spec.Budget > 0 && spec.Effort == "" {
		spec.Effort = qoderEffortForBudget(spec.Budget)
	}
	// 显式关闭压倒一切
	if spec.Enabled != nil && !*spec.Enabled {
		spec.Effort = "none"
	}

	// 默认：该模型支持的最高档（默认最强思考）
	if !explicit && spec.Effort == "" && entry != nil {
		spec.Effort = qoderHighestEffort(entry.Efforts, entry.DefaultEffort)
	}

	// 不支持的档位就近钳制（|距离|最小；同距取更高档），绝不发非法值
	if spec.Effort != "" && spec.Effort != "none" && entry != nil && len(entry.Efforts) > 0 {
		ok := false
		for _, e := range entry.Efforts {
			if e == spec.Effort {
				ok = true
				break
			}
		}
		if !ok {
			spec.Effort = qoderClampEffort(spec.Effort, entry)
		}
	}
	return spec
}

// qoderHighestEffort 取支持档位中的最高档；无档位信息时回退 defaultEffort。
func qoderHighestEffort(efforts []string, defaultEffort string) string {
	best := ""
	for _, e := range efforts {
		if qoderEffortRank(e) > qoderEffortRank(best) {
			best = e
		}
	}
	if best == "" {
		return defaultEffort
	}
	return best
}

// qoderClampEffort 就近钳制：与所请求档位 |距离| 最小的支持档；同距取更高档。
func qoderClampEffort(effort string, entry *qoderModelEntry) string {
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

// qoderEffortRank 强度序号（none=0 … max=5）；未知档位 = -1。
func qoderEffortRank(e string) int {
	for i, name := range qoderEffortOrder {
		if name == e {
			return i
		}
	}
	return -1
}

// ---------- 可见思考提示（可选兜底，默认关闭） ----------
//
// 信封对齐后原生 reasoning_content 是唯一可靠杠杆（dsh-provider-qoder 不注入
// 任何提示词）。提示词层只作为极端兜底保留：QODER_THINK_PROMPT=on 显式开启。
// 历史教训：默认注入会扭曲输出格式（「干什么活都干杂」），且带 tools 的
// Agent 请求模型根本不遵守，两头不讨好。

// qoderThinkingPrompt 可见思考指令（仅 QODER_THINK_PROMPT=on 时注入）。
const qoderThinkingPrompt = "除了一两个词就能回答的极简问题，回答前必须先把完整的推理过程写在 <think> 和" +
	qoderThinkClose + " 之间：明确关键假设、拆解问题、逐项推演并自我检验结论，" + qoderThinkClose +
	" 之后再给最终答案，最终答案只输出用户需要的内容，保持用户要求的格式" +
	"（代码、清单、工具调用 JSON 等原样保留），不要把思考内容混入其中；极简问题可以直接作答。"

// qoderThinkClose 闭合思考标签（拼接构造，规避编辑链路对标签字面量的截断）。
const qoderThinkClose = "</think>"

// qoderThinkPromptEnabled 环境变量总开关（默认关；QODER_THINK_PROMPT=on 开启）。
func qoderThinkPromptEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("QODER_THINK_PROMPT")))
	return v == "on" || v == "1" || v == "true"
}

// qoderInjectPrompt 把提示词并入第一个 system 消息（没有则前置一条）。
func qoderInjectPrompt(body map[string]any, prompt string) {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "system" {
			switch c := mm["content"].(type) {
			case string:
				mm["content"] = c + "\n\n" + prompt
			case []any: // 视觉消息等多段 content
				mm["content"] = append(c, map[string]any{"type": "text", "text": prompt})
			default:
				mm["content"] = prompt
			}
			return
		}
	}
	sys := map[string]any{"role": "system", "content": prompt}
	body["messages"] = append([]any{sys}, msgs...)
}

// entryFor 按真实 key 查目录条目（未收录返回 nil）。
func (p *QoderProxy) entryFor(key string) *qoderModelEntry {
	cat := p.catalogSnapshot()
	for i := range cat {
		if cat[i].Key == key {
			return &cat[i]
		}
	}
	return nil
}

// qoderNoThinkMarked 判断最后一条用户消息是否带关闭标记。
func qoderNoThinkMarked(msgs []any) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r != "user" {
			continue
		}
		if c, ok := mm["content"].(string); ok {
			return strings.Contains(c, qoderNoThinkMarker) || strings.Contains(c, qoderNoThinkMarkerAlt)
		}
		return false
	}
	return false
}

// qoderStripNoThinkMarker 从最后一条用户消息里摘除关闭标记（不透传给上游）。
func qoderStripNoThinkMarker(msgs []any) {
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r != "user" {
			continue
		}
		if c, ok := mm["content"].(string); ok {
			if strings.Contains(c, qoderNoThinkMarker) || strings.Contains(c, qoderNoThinkMarkerAlt) {
				c = strings.ReplaceAll(c, qoderNoThinkMarker, "")
				c = strings.ReplaceAll(c, qoderNoThinkMarkerAlt, "")
				mm["content"] = strings.TrimSpace(c)
			}
		}
		return // 只看最后一条用户消息
	}
}
