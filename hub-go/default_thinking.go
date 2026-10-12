package main

// 未指定思考档位时默认拉满（2026-10-08 用户要求）。
//
// 用户原话（m00298）：「我在 dsh 中调用的时候都没看到思考等级的调节地方啊，
// 如果我没发送思考等级那就默认最高就行，所有通过 hub-go 的都是这样设置即可。」
//
// 各线路落地点：
//   - Qoder：qoder_thinking.go 的 qoderResolveThinking（已有，取 Efforts 最高档）
//   - Zcode ：zcode_thinking.go 的 zcodeHighestEffort（本次改，原来取目录默认档）
//   - WorkBuddy dom/intl 与 MiMo：都走 Proxy.handleChat，本文件在这里补默认值
//   - Trae：独立协议（TraeProxy.handleChat），上游档位未公开，保持原样
//
// 与 Qoder/Zcode 一致的边界：只要客户端表达了任何思考意图（档位、开关、budget、
// 或消息里的 [no-thinking] 标记），一律尊重客户端，绝不改写；只有「什么都没提」
// 时才补默认档。WorkBuddy 后端不透出每个模型的档位目录，所以用规范序里最高的
// 具名档 "max"（实测 glm-5.1 / glm-5.3 / deepseek-v4-flash / minimax-m3-pay / auto
// 接受该字段且返回正常，不支持的模型会忽略它，不会报错）。

// wbThinkingDefaultEffort 是客户端未表达意图时下发的档位。
const wbThinkingDefaultEffort = "max"

// thinkingIntentKeys 是客户端可能用来表达思考意图的字段名。
// 出现任何一个（不论值）都算「已表达」，hub-go 不越权改写。
var thinkingIntentKeys = []string{
	"reasoning_effort", "effort", "thinking", "thinking_config",
	"reasoning", "enable_thinking", "reasoning_enabled",
	"reasoning_budget_tokens", "thinking_budget", "preserve_thinking",
}

// hasThinkingIntent 判断请求体是否已经表达过思考意图。
func hasThinkingIntent(raw map[string]any) bool {
	for _, k := range thinkingIntentKeys {
		if v, ok := raw[k]; ok && !thinkingValueIsNil(v) {
			return true
		}
	}
	return false
}

// thinkingValueIsNil 允许客户端显式发 null 表示「不关心」，视同没发。
func thinkingValueIsNil(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	default:
		return false
	}
}

// applyDefaultThinking 在客户端没提思考档位时补一个最高档。
// 尊重 [no-thinking] 标记：那是客户端主动关思考，不是「没说」。
// defaultEffort 为空表示该线路不注入（上游档位语义未知时保持原样透传）。
func applyDefaultThinking(body, raw map[string]any, msgs []any, defaultEffort string) {
	if defaultEffort == "" {
		return
	}
	if _, ok := body["reasoning_effort"]; ok {
		return // 已由 passthrough 带上客户端的显式值
	}
	if hasThinkingIntent(raw) || qoderNoThinkMarked(msgs) {
		return
	}
	body["reasoning_effort"] = defaultEffort
}
