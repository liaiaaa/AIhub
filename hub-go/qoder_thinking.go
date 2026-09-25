package main

// 思考控制：effort 解析、注入与诊断
// 由 proxy_qoder.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ---------- 思考控制 ----------
//
// 背景（2026-09-24 破解定论，来源：官方 worker runtime 源码 + proto/chat.proto
// + 官方模型目录 thinking_config 三方交叉验证）：
//  1. Qoder 网关**有**真实思考强度控制，字段在请求体 `parameters` 对象内
//     （官方 runtime 的请求组装函数把它放在 parameters，而不是顶层）：
//       parameters.reasoning_effort        = none|low|medium|high|xhigh|max
//       parameters.enable_thinking         = true|false
//       parameters.reasoning_budget_tokens = N（另有别名 thinking_budget）
//       parameters.preserve_thinking       = true|false
//     响应侧 thinking 以 delta.reasoning_content 流回（proto ResponseChatMessage）。
//  2. 每个模型支持的强度档与默认档来自官方目录 thinking_config.efforts
//     （如 qfmodel：low/medium/xhigh，默认 medium；gmodel：low/high/max，默认 max）。
//     官方客户端**每个请求都发送** parameters.reasoning_effort（未设置时取目录默认档）。
//  3. 历史教训：此前的“字段全部无效”结论是**无效对照**得出的——所有 A/B 实验恰好
//     都测在模型默认值上（gmodel 默认即 max），或把字段放在顶层/写成驼峰
//     （parameters.reasoningEffort），因此看不出差异。
//
// 本节把客户端的思考意图（reasoning_effort / thinking / enable_thinking /
// thinking_budget / 消息内标记）翻译成上述 parameters 字段，默认取目录默认档
// （= 默认真思考）。旧的“注入强制 <think> 格式系统提示词”方案已废弃：它不产生真思考，
// 还扭曲输出格式，是“干什么活都干杂”的元凶。

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
	Effort   string // none|low|medium|high|xhigh|max；空 = 不发送
	Enabled  *bool  // enable_thinking；nil = 不发送
	Budget   int    // reasoning_budget_tokens；0 = 不发送
	Preserve *bool  // preserve_thinking；nil = 不发送
}

// qoderResolveThinking 解析客户端意图（优先级：消息内标记 > 请求体显式字段 >
// 目录默认档），并做两步规范化：
//   - budget → 强度档换算（未显式给档位时）
//   - 档位不在该模型支持列表时回退目录默认档（绝不发非法值）
func qoderResolveThinking(body map[string]any, entry *qoderModelEntry) qoderThinkingSpec {
	spec := qoderThinkingSpec{}
	explicit := false

	// 1) 显式强度字段（OpenAI 风格 reasoning_effort / 简写 effort）
	for _, k := range []string{"reasoning_effort", "effort"} {
		if s, ok := body[k].(string); ok {
			if e := qoderNormalizeEffort(s); e != "" {
				spec.Effort = e
				explicit = true
				break
			}
		}
	}

	// 2) thinking:{type,budget_tokens}（DeepSeek 风格）
	if th, ok := body["thinking"].(map[string]any); ok {
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
	if v, ok := body["enable_thinking"].(bool); ok {
		explicit = true
		spec.Enabled = &v
	}
	if b, ok := qoderIntVal(body["reasoning_budget_tokens"]); ok {
		explicit = true
		spec.Budget = b
	} else if b, ok := qoderIntVal(body["thinking_budget"]); ok {
		explicit = true
		spec.Budget = b
	}
	if v, ok := body["preserve_thinking"].(bool); ok {
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

	// 默认：该模型支持的最高档（2026-09-24 用户选择：默认最强思考）
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

// qoderApplyThinking 按用户意图把思考参数写进 body["parameters"]（官方 wire 形态），
// 并把已翻译的顶层控制字段摘掉（防双重语义）。除注入温和可见思考提示外不改动 messages。
func qoderApplyThinking(body map[string]any, entry *qoderModelEntry) {
	// 消息内标记 = 本次明确不要思考（最具体，优先级最高）
	marked := qoderNoThinkMarked(body)
	qoderStripNoThinkMarker(body)
	spec := qoderThinkingSpec{Effort: "none"}
	if !marked {
		spec = qoderResolveThinking(body, entry)
	}

	params := map[string]any{}
	if p, ok := body["parameters"].(map[string]any); ok {
		for k, v := range p {
			params[k] = v
		}
	}
	switch {
	case spec.Effort == "none":
		params["reasoning_effort"] = "none"
		params["enable_thinking"] = false
		delete(params, "reasoning_budget_tokens")
	case spec.Effort != "":
		params["reasoning_effort"] = spec.Effort
		params["enable_thinking"] = true
		if spec.Budget > 0 {
			params["reasoning_budget_tokens"] = spec.Budget
		}
	case spec.Enabled != nil:
		params["enable_thinking"] = *spec.Enabled
		if spec.Budget > 0 {
			params["reasoning_budget_tokens"] = spec.Budget
		}
	case spec.Budget > 0:
		params["reasoning_budget_tokens"] = spec.Budget
	}
	if spec.Preserve != nil {
		params["preserve_thinking"] = *spec.Preserve
	}
	if len(params) > 0 {
		body["parameters"] = params
	}
	// 顶层控制字段已翻译进 parameters，摘掉防双重语义/上游噪音
	for _, k := range []string{
		"reasoning_effort", "effort", "thinking", "enable_thinking",
		"thinking_budget", "reasoning_budget_tokens", "preserve_thinking",
	} {
		delete(body, k)
	}

	// 可见思考提示：此网关的隐藏思考在裸 HTTP 路径上不可验证（2026-09-24 实测
	// parameters 各形态均无可观测差异），可见推理的可靠杠杆仍是提示词层。
	// 因此默认注入**温和版**（qoderThinkingPrompt），但：
	//   - 显式关闭 / [no-thinking] 标记 → 不注入；
	//   - 带 tools 的 Agent 请求同样注入（用户拍板 2026-09-24），由提示词保护工具调用格式；
	//   - 环境变量 QODER_THINK_PROMPT=off → 全局关闭。
	off := spec.Effort == "none" || (spec.Enabled != nil && !*spec.Enabled)
	if off || qoderThinkPromptDisabled() {
		return
	}
	qoderInjectPrompt(body, qoderThinkingPrompt)
}

// qoderThinkDiag 思考链路一行诊断：把客户端意图→翻译→注入全过程可视化，
// 用于定位"思考框短/没真思考"断在客户端意图、目录档位还是参数注入哪一环。
func qoderThinkDiag(raw, body map[string]any, entry *qoderModelEntry, marked bool) string {
	var b strings.Builder
	b.WriteString("意图{")

	// 1) 客户端显式意图（原始请求体，翻译前）
	first := true
	writeKV := func(k string, v any) {
		if !first {
			b.WriteString(" ")
		}
		first = false
		fmt.Fprintf(&b, "%s=%v", k, v)
	}
	for _, k := range []string{"reasoning_effort", "effort", "enable_thinking",
		"reasoning_budget_tokens", "thinking_budget", "preserve_thinking"} {
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
		b.WriteString("无显式字段(取目录默认档)")
	}
	b.WriteString("}")

	// 2) 目录条目（决定默认档与合法档位）
	if entry == nil {
		b.WriteString(" 目录{未命中}")
	} else {
		fmt.Fprintf(&b, " 目录{efforts=%v default=%s disabled=%v}",
			entry.Efforts, entry.DefaultEffort, entry.SupportsDisabled)
	}

	// 3) 实际注入的 parameters（翻译结果，真正发给上游的思考旋钮）
	fmt.Fprintf(&b, " → parameters{%s}", qoderParamsDiag(body))
	return b.String()
}

// qoderParamsDiag 渲染 body["parameters"] 里与思考相关的字段。
func qoderParamsDiag(body map[string]any) string {
	p, ok := body["parameters"].(map[string]any)
	if !ok {
		return "无 parameters 对象"
	}
	var parts []string
	for _, k := range []string{"reasoning_effort", "enable_thinking",
		"reasoning_budget_tokens", "preserve_thinking"} {
		if v, ok := p[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) == 0 {
		return "思考字段全缺"
	}
	return strings.Join(parts, " ")
}

// qoderThinkingPrompt 可见思考指令：推理写在 <think> 内（代理转写成
// reasoning_content，客户端渲染为折叠思考框），同时明确保护最终输出格式。
// 措辞平衡（2026-09-24 实测教训）：必须强制写思考标签（温和措辞实测 3/3 被
// 模型忽略，可见思考失效）；但不约束答案结构（旧版刚性两段式措辞会扭曲
// 一切输出格式，干活必乱），只保护用户要求的格式。
const qoderThinkingPrompt = "除了一两个词就能回答的极简问题，回答前必须先把完整的推理过程写在 <think> 和" +
	qoderThinkClose + " 之间：明确关键假设、拆解问题、逐项推演并自我检验结论，" + qoderThinkClose +
	" 之后再给最终答案，最终答案只输出用户需要的内容，保持用户要求的格式" +
	"（代码、清单、工具调用 JSON 等原样保留），不要把思考内容混入其中；极简问题可以直接作答。"

// qoderThinkClose 闭合思考标签（拼接构造，规避编辑链路对标签字面量的截断）。
const qoderThinkClose = "</think>"

// qoderThinkPromptDisabled 环境变量总开关（QODER_THINK_PROMPT=off）。
func qoderThinkPromptDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("QODER_THINK_PROMPT")))
	return v == "off" || v == "0" || v == "false"
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
func qoderNoThinkMarked(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
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
func qoderStripNoThinkMarker(body map[string]any) {
	msgs, _ := body["messages"].([]any)
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

