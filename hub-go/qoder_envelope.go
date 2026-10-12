package main

// qodercli 请求信封：对齐官方客户端 / dsh-provider-qoder 的 wire 形态。
//
// 背景（2026-09-25 根因定论）：此前把裸 OpenAI 体直接发给 agent_chat_generation，
// 网关走兜底解析——chat/model 等字段能被容错认出，但思考配置整条链路被忽略：
// parameters.reasoning_effort / enable_thinking 无论怎么发都无观测差异，
// 原生 reasoning_content 从不流回（日志常年 think=0帧），只能靠提示词注入
// <think>（带 tools 的 Agent 请求模型直接无视）。
//
// 对齐信封后，思考开关在顶层 model_config.is_reasoning（chat_context.extra.
// modelConfig 同步），强度在 parameters.reasoning_effort。参考实现：
// github.com/mo-n/dsh-provider-qoder（其参考 pi-provider-qoder）。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// qoderChatMaxTokens 上游默认/上限输出 token（插件 defaultMaxTokens 同值）。
const qoderChatMaxTokens = 32768

// qoderUUID 生成 UUID v4（信封各 id 用）。
func qoderUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化用时间戳保证 id 非空
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// qoderSessionID 会话 id：用户维度的稳定前缀 + 每请求随机段。
// 官方客户端用它聚合 Credits 面板的单次运行消耗；代理无真实会话状态，
// 采用插件在「无会话」路径的等价做法（每请求新 id）。
func qoderSessionID(uid string) string {
	h := sha256.New()
	h.Write([]byte("qoder-session"))
	h.Write([]byte{0})
	h.Write([]byte(uid))
	return hex.EncodeToString(h.Sum(nil)[:8]) + "-" + qoderUUID()
}

// qoderLastUserText 取最后一条 user 消息的纯文本（多段 content 取 text 部分拼接）。
func qoderLastUserText(msgs []any) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r != "user" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return c
		case []any:
			var b []byte
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if s, ok := pm["text"].(string); ok {
						b = append(b, s...)
					}
				}
			}
			return string(b)
		default:
			return ""
		}
	}
	return ""
}

// qoderClientMaxTokens 读客户端 max_tokens / max_completion_tokens，
// 钳制到 [1, 模型上限]，缺省取模型上限（目录 max_output_tokens，缺省 32768）。
func qoderClientMaxTokens(raw map[string]any, entry *qoderModelEntry) int {
	limit := qoderChatMaxTokens
	if entry != nil && entry.MaxOutputTokens > 0 {
		limit = entry.MaxOutputTokens
	}
	n := 0
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := qoderIntVal(raw[k]); ok && v > 0 {
			n = v
			break
		}
	}
	if n <= 0 {
		return limit
	}
	if n > limit {
		return limit
	}
	return n
}

// qoderEffortToWire 把解析出的思考意图映射到信封 wire 字段。
//   - effort 为 none/空：不发 reasoning_effort，is_reasoning=false（显式关闭）
//     或回退目录 IsReasoning（无显式意图、模型无强度档时）。
//   - 其余：发 parameters.reasoning_effort，is_reasoning=true。
//
// 官方 wire 无 parameters.enable_thinking（此前是我方臆测字段，已废弃）。
func qoderEffortToWire(spec qoderThinkingSpec, entry *qoderModelEntry) (effort string, isReasoning bool) {
	if spec.Effort != "" && spec.Effort != "none" {
		return spec.Effort, true
	}
	if spec.Effort == "none" {
		return "", false
	}
	// 无显式意图、模型无强度档（如 auto / mmodel）：跟随目录 is_reasoning
	if entry != nil {
		return "", entry.IsReasoning
	}
	// 目录未收录但客户端显式开了 thinking（enable_thinking=true 等）
	if spec.Enabled != nil && *spec.Enabled {
		return "", true
	}
	return "", false
}

// qoderEnvelope 构建 qodercli 信封。openai 里的控制字段（model/stream/思考字段/
// max_tokens 等）不整体透传——官方信封字段是白名单形态，多余字段属于网关
// 兜底才容忍的噪音；messages/tools 原样带入（含客户端回传的
// reasoning_content，等价插件的 preserveThinking=true）。
func qoderEnvelope(messages []any, tools any, modelKey, uid string, spec qoderThinkingSpec, entry *qoderModelEntry, raw map[string]any) map[string]any {
	effort, isReasoning := qoderEffortToWire(spec, entry)
	maxTokens := qoderClientMaxTokens(raw, entry)

	params := map[string]any{"max_tokens": maxTokens}
	if effort != "" {
		params["reasoning_effort"] = effort
	}

	lastUser := qoderLastUserText(messages)
	requestID := qoderUUID()

	env := map[string]any{
		"request_id":       requestID,
		"request_set_id":   "qoder-request-" + qoderUUID(),
		"chat_record_id":   requestID,
		"session_id":       qoderSessionID(uid),
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"code_language":    "",
		"chat_prompt":      "",
		"image_urls":       nil,
		"aliyun_user_type": "",
		"system":           "",
		"messages":         messages,
		"parameters":       params,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"imageUrls":  nil,
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": isReasoning},
				"originalContent": lastUser,
			},
			"features": []any{},
			"text":     lastUser,
		},
		"model_config": func() map[string]any {
			mc := map[string]any{
				"key":               modelKey,
				"is_reasoning":      isReasoning,
				"max_output_tokens": maxTokens,
				"source":            "system",
			}
			// 上下文档位：默认选最大档（1M），对齐 dsh-provider-qoder 的 context_config
			// 形态与用户的 1M 会话配置；QODER_CONTEXT_TIER=none 可回退不发（诊断用）。
			if cc := qoderPickContextTier(entry); cc != nil {
				mc["context_config"] = cc
			}
			return mc
		}(),
		"business": map[string]any{
			"product":  "cli",
			"version":  "1.0.60",
			"type":     "agent",
			"stage":    "start",
			"id":       qoderUUID(),
			"name":     truncate(lastUser, 30),
			"begin_at": time.Now().UnixMilli(),
		},
	}
	if tools != nil {
		env["tools"] = tools
	}
	return env
}

// qoderWireDiag 思考链路一行诊断：客户端意图 → 目录 → 实际 wire 字段。
func qoderWireDiag(raw map[string]any, msgs []any, entry *qoderModelEntry, marked bool, env map[string]any) string {
	var b []byte
	add := func(sep string, s string) {
		b = append(b, sep...)
		b = append(b, s...)
	}
	first := true
	add("", "意图{")
	for _, k := range []string{"reasoning_effort", "effort", "enable_thinking",
		"reasoning_budget_tokens", "thinking_budget", "preserve_thinking"} {
		if v, ok := raw[k]; ok {
			if !first {
				add(" ", "")
			}
			first = false
			add("", fmt.Sprintf("%s=%v", k, v))
		}
	}
	if th, ok := raw["thinking"].(map[string]any); ok {
		if !first {
			add(" ", "")
		}
		first = false
		add("", fmt.Sprintf("thinking=%v", th))
	}
	if marked {
		if !first {
			add(" ", "")
		}
		first = false
		add("", "[no-thinking]=true")
	}
	if first {
		add("", "无显式字段(取目录最高档)")
	}
	add("", "}")

	if entry == nil {
		add(" 目录{未命中}", "")
	} else {
		add("", fmt.Sprintf(" 目录{efforts=%v default=%s disabled=%v}",
			entry.Efforts, entry.DefaultEffort, entry.SupportsDisabled))
	}

	mc, _ := env["model_config"].(map[string]any)
	pm, _ := env["parameters"].(map[string]any)
	add("", fmt.Sprintf(" → wire{is_reasoning=%v reasoning_effort=%v max_tokens=%v}",
		mc["is_reasoning"], pm["reasoning_effort"], pm["max_tokens"]))
	return string(b)
}
