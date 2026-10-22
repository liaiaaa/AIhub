package main

// Anthropic 入站转换（2026-10-13 融合改造）：把 Claude Code / Claude Desktop 的
// /v1/messages 请求翻译成各线路的 chat/completions，响应再翻译回 Anthropic 形态。
//
// 这是「hub-go 吞并 Hub Switch 网关职责」的关键一块：有了它，Claude 客户端可以
// 直接指向 hub-go 统一网关（:8790），不再需要 Hub Switch :15721 做协议转换。
//
// 形态覆盖：
//   - 非流式：整包转换（解析上游 chat completion → Anthropic message 对象）；
//   - 流式：逐帧转换（chat SSE → message_start / content_block_* / message_delta /
//     message_stop），增量吐给客户端，不做假流式；
//   - 工具调用：Anthropic tools/tool_use/tool_result ↔ chat tools/tool_calls/tool
//     消息双向映射；图片（base64）→ data URL；thinking 请求透传为 thinking 块。
//
// 思考口径：请求里带 "thinking"（Claude Code 思考开启时才发）才输出 thinking 块；
// 否则丢弃上游 reasoning_content（各线路 ThinkingDefault 注入的思考不外泄，
// 免得客户端没开思考却收到 thinking 块报错）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// aggHTTPClient 统一网关到各线路本地端口的回环客户端（线路端口只听 127.0.0.1）。
func aggHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

// ---------- 请求侧：Anthropic → chat ----------

// anthropicToChatBody 把 /v1/messages 请求体转成 chat/completions 请求体。
// 返回的 body 里 model 已是线路真实模型名（由调用方改写）。
func anthropicToChatBody(req map[string]any) (map[string]any, error) {
	body := map[string]any{"messages": []any{}}
	msgs := []any{}

	// system：字符串或块数组 → 单条 system 消息
	if sys, ok := req["system"]; ok {
		if text := anthropicTextOf(sys); strings.TrimSpace(text) != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": text})
		}
	}

	// messages：user/assistant 交替；tool_result 出现在 user 消息里，
	// 必须转成紧随 assistant(tool_calls) 的 role=tool 消息。
	rawMsgs, _ := req["messages"].([]any)
	for _, rm := range rawMsgs {
		m, _ := rm.(map[string]any)
		role, _ := m["role"].(string)
		content := m["content"]
		switch role {
		case "user":
			msgs = append(msgs, anthropicUserToChat(content)...)
		case "assistant":
			msgs = append(msgs, anthropicAssistantToChat(content)...)
		}
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	body["messages"] = msgs

	// 标量参数
	if v, ok := req["max_tokens"]; ok {
		body["max_tokens"] = v
	} else {
		body["max_tokens"] = 4096
	}
	for _, k := range []string{"temperature", "top_p", "user"} {
		if v, ok := req[k]; ok {
			body[k] = v
		}
	}
	if ss, ok := req["stop_sequences"].([]any); ok && len(ss) > 0 {
		body["stop"] = ss
	}

	// tools：{name,description,input_schema} → chat function tools
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		chatTools := make([]any, 0, len(tools))
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			name, _ := tm["name"].(string)
			if name == "" {
				continue
			}
			entry := map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name, "parameters": orEmptyObj(tm["input_schema"])},
			}
			if d, ok := tm["description"].(string); ok {
				// 2026-10-21 开源审查：补 ok 断言。entry["function"] 是本函数
				// 刚构造的所以当前安全，但裸断言一旦上游结构变化就是 panic。
				if fm, ok := entry["function"].(map[string]any); ok {
					fm["description"] = d
				}
			}
			chatTools = append(chatTools, entry)
		}
		if len(chatTools) > 0 {
			body["tools"] = chatTools
		}
	}
	// tool_choice：auto / any / {type:tool,name}
	if tc, ok := req["tool_choice"].(map[string]any); ok {
		t, _ := tc["type"].(string)
		switch t {
		case "auto":
			body["tool_choice"] = "auto"
		case "any":
			body["tool_choice"] = "required"
		case "tool":
			n, _ := tc["name"].(string)
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": n}}
		}
	}

	// 思考档位：Anthropic 请求没有 effort 概念；开了 thinking 就交给各线路
	// ThinkingDefault 注入最高档（与其它客户端一致），不显式发 reasoning_effort。
	//
	// 上游一律用流式（与聚合口 responses→chat 转换同口径）：
	//   - 客户端要流式 → 逐帧转换成 Anthropic SSE；
	//   - 客户端要非流式 → collectStream 聚合成一个 message 对象。
	// 此前把客户端的 stream=false 透传给上游，导致非流式回包是 JSON 而
	// 解析器只认 SSE，实测整条响应为空（2026-10-13）。
	body["stream"] = true
	body["stream_options"] = map[string]any{"include_usage": true}
	return body, nil
}

func orEmptyObj(v any) any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// anthropicTextOf 拼接字符串或块数组里的全部 text。
func anthropicTextOf(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, blk := range v {
			if m, ok := blk.(map[string]any); ok {
				if m["type"] == "text" {
					if s, ok := m["text"].(string); ok {
						b.WriteString(s)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}

// anthropicUserToChat user 消息：tool_result → role=tool；text/image → user。
func anthropicUserToChat(content any) []any {
	out := []any{}
	// 先收集 tool_result（必须整体排在最前），再放其余内容
	var rest []any
	if s, ok := content.(string); ok {
		rest = append(rest, map[string]any{"type": "text", "text": s})
	} else if blocks, ok := content.([]any); ok {
		for _, blk := range blocks {
			m, _ := blk.(map[string]any)
			if m == nil {
				continue
			}
			if m["type"] == "tool_result" {
				id, _ := m["tool_use_id"].(string)
				text := anthropicTextOf(m["content"])
				if isErr, _ := m["is_error"].(bool); isErr {
					text = "ERROR: " + text
				}
				out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
				continue
			}
			rest = append(rest, m)
		}
	}
	if len(rest) > 0 {
		out = append(out, map[string]any{"role": "user", "content": anthropicBlocksToChatContent(rest)})
	}
	return out
}

// anthropicAssistantToChat assistant 消息：text → content；tool_use → tool_calls。
func anthropicAssistantToChat(content any) []any {
	text := ""
	var toolCalls []any
	if s, ok := content.(string); ok {
		text = s
	} else if blocks, ok := content.([]any); ok {
		for _, blk := range blocks {
			m, _ := blk.(map[string]any)
			if m == nil {
				continue
			}
			switch m["type"] {
			case "text":
				if s, ok := m["text"].(string); ok {
					text += s
				}
			case "tool_use":
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				args, _ := json.Marshal(orEmptyObj(m["input"]))
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{"name": name, "arguments": string(args)},
				})
			}
			// thinking 块丢弃：上游不需要回放思考历史
		}
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if text != "" {
		msg["content"] = text
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return []any{msg}
}

// anthropicBlocksToChatContent 块数组 → chat content（纯文本给字符串，多模态给数组）。
func anthropicBlocksToChatContent(blocks []any) any {
	parts := make([]any, 0, len(blocks))
	texts := 0
	for _, blk := range blocks {
		m, _ := blk.(map[string]any)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "text":
			if s, ok := m["text"].(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": s})
				texts++
			}
		case "image":
			src, _ := m["source"].(map[string]any)
			mt, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			if src != nil && src["type"] == "base64" && data != "" {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + mt + ";base64," + data},
				})
			}
		}
	}
	if len(parts) == 1 && texts == 1 {
		if p, ok := parts[0].(map[string]any); ok {
			if s, ok := p["text"].(string); ok {
				return s
			}
		}
	}
	return parts
}

// ---------- 路由 ----------

// aggAnthropicMessages /v1/messages 统一入口。
func (h *Hub) aggAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad json: "+err.Error())
		return
	}
	slug, _ := req["model"].(string)
	am, ok := resolveAggModel(slug)
	if !ok {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("未知模型 %q（可用形式：线路.模型 / ccs-线路/模型，或在控制台白名单里添加）", slug))
		return
	}
	if !h.lineRunning(am.Line) {
		anthropicError(w, http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("模型 %s 在 %s 线路上，但该线路未启动：请先在 AIHub 控制台启动它", am.Backend, aggLineCN[am.Line]))
		return
	}
	chatBody, err := anthropicToChatBody(req)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	chatBody["model"] = am.Backend
	nb, err := json.Marshal(chatBody)
	if err != nil {
		anthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	tap := &usageTap{w: w}
	w = tap
	t0 := time.Now()
	defer func() { recordTap(tap, am, "messages", time.Since(t0).Milliseconds()) }()

	clientStream, _ := req["stream"].(bool)
	wantThinking := anthropicThinkingOn(req)
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", aggLinePort[am.Line])
	httpReq, err := http.NewRequestWithContext(r.Context(), "POST", url, strings.NewReader(string(nb)))
	if err != nil {
		anthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := aggHTTPClient().Do(httpReq)
	if err != nil {
		anthropicError(w, http.StatusBadGateway, "api_error", "上游连接失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		anthropicError(w, resp.StatusCode, anthropicErrType(resp.StatusCode), extractChatErrMsg(b, resp.Status))
		return
	}
	if clientStream {
		anthropicStreamResponse(w, resp.Body, slug, wantThinking)
		return
	}
	// 全量读取后喂 collectStream（早期版本 4KB 截断会造成假空响应）。
	// 2026-10-21 开源审查：原来无上限 ReadAll + 每个请求都打一条诊断日志
	// （含响应尾部内容，既污染日志又不该长期留在生产路径）。加上限并删诊断。
	const anthropicRespMax = 64 << 20
	full, rerr := io.ReadAll(io.LimitReader(resp.Body, anthropicRespMax))
	if rerr != nil {
		logHub("anthropic 非流式读取失败: %v", rerr)
	}
	anthropicNonStreamResponse(w, strings.NewReader(string(full)), slug, wantThinking)
}

// anthropicThinkingOn 客户端是否显式开了思考。
func anthropicThinkingOn(req map[string]any) bool {
	if t, ok := req["thinking"].(map[string]any); ok {
		s, _ := t["type"].(string)
		return s == "enabled"
	}
	return false
}

func anthropicErrType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusServiceUnavailable || status == 529:
		return "overloaded_error"
	case status == 401 || status == 403:
		return "authentication_error"
	}
	return "api_error"
}

// extractChatErrMsg 从上游 chat 错误体里抠出人类可读的一句话。
func extractChatErrMsg(b []byte, fallback string) string {
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if e, ok := m["error"].(map[string]any); ok {
			if s, ok := e["message"].(string); ok && s != "" {
				return s
			}
		}
		if s, ok := m["msg"].(string); ok && s != "" {
			return s
		}
	}
	s := string(b)
	if len(s) > 300 {
		s = s[:300]
	}
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// anthropicError Anthropic 错误信封。
func anthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// ---------- 响应侧（非流式） ----------

func anthropicNonStreamResponse(w http.ResponseWriter, body io.Reader, slug string, wantThinking bool) {
	col := collectStream(body)
	env := col.build() // chat completion 信封：choices[0].message / usage / finish_reason

	// 解包信封（2026-10-13 修复：此前误把信封当裸 message 解包，
	// content/tool_calls 全取空；usage 走信封顶层，字段是 json.Number）
	msg := map[string]any{}
	finish := ""
	// 2026-10-21 开源审查：先判长度再取下标 —— 上游返回空 choices（纯错误
	// 响应、被截断）时，直接 [0] 会 panic 掉整个 handler。
	choicesList := asSlice(env["choices"])
	if len(choicesList) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "upstream_error",
				"message": "上游没有返回任何 choices",
			},
		})
		return
	}
	if ch, ok := choicesList[0].(map[string]any); ok {
		if m, ok := ch["message"].(map[string]any); ok {
			msg = m
		}
		if f, ok := ch["finish_reason"].(string); ok {
			finish = f
		}
	}
	usage, _ := env["usage"].(map[string]any)

	content := []any{}
	if wantThinking {
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": rc})
		}
	}
	if s, ok := msg["content"].(string); ok && s != "" {
		content = append(content, map[string]any{"type": "text", "text": s})
	}
	for _, tc := range asSlice(msg["tool_calls"]) {
		tcm, _ := tc.(map[string]any)
		fn, _ := tcm["function"].(map[string]any)
		input := map[string]any{}
		if args, ok := fn["arguments"].(string); ok && strings.TrimSpace(args) != "" {
			_ = json.Unmarshal([]byte(args), &input)
		}
		id, _ := tcm["id"].(string)
		name, _ := fn["name"].(string)
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	inTok, outTok := anthropicUsage(usage)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_" + randHex(12),
		"type":          "message",
		"role":          "assistant",
		"model":         slug,
		"content":       content,
		"stop_reason":   anthropicStopReason(finish),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inTok,
			"output_tokens": outTok,
		},
	})
}

func anthropicStopReason(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func anthropicUsage(usage map[string]any) (int, int) {
	in := int(floatVal(usage["prompt_tokens"]))
	out := int(floatVal(usage["completion_tokens"]))
	return in, out
}

func floatVal(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// ---------- 响应侧（流式：chat SSE → Anthropic SSE，逐帧增量） ----------

type anthStreamState struct {
	w          io.Writer
	flusher    http.Flusher
	model      string
	blockIdx   int
	textOpen   bool
	thinkOpen  bool
	toolBlocks map[int]int // chat tool index → anthropic block idx
	toolNames  map[int]string
	stopReason string
	inTok      int
	outTok     int
}

func anthropicStreamResponse(w http.ResponseWriter, body io.Reader, slug string, wantThinking bool) {
	flusher, _ := w.(http.Flusher)
	st := &anthStreamState{w: w, flusher: flusher, model: slug, toolBlocks: map[int]int{}, toolNames: map[int]string{}}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	st.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + randHex(12), "type": "message", "role": "assistant",
			"model": slug, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "[DONE]" {
			break
		}
		var chunk map[string]any
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			st.inTok = int(floatVal(u["prompt_tokens"]))
			st.outTok = int(floatVal(u["completion_tokens"]))
		}
		for _, ch := range asSlice(chunk["choices"]) {
			cm, _ := ch.(map[string]any)
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				st.stopReason = anthropicStopReason(fr)
			}
			delta, _ := cm["delta"].(map[string]any)
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" && wantThinking {
				st.openThinking()
				st.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": st.blockIdx,
					"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
				})
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				st.openText()
				st.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": st.blockIdx,
					"delta": map[string]any{"type": "text_delta", "text": ct},
				})
			}
			for _, tc := range asSlice(delta["tool_calls"]) {
				tcm, _ := tc.(map[string]any)
				idx := int(floatVal(tcm["index"]))
				fn, _ := tcm["function"].(map[string]any)
				if id, ok := tcm["id"].(string); ok && id != "" {
					st.toolNames[idx] = id
				}
				name := ""
				if n, ok := fn["name"].(string); ok {
					name = n
				}
				if _, open := st.toolBlocks[idx]; !open {
					st.closeOpenBlocks()
					st.blockIdx++
					st.toolBlocks[idx] = st.blockIdx
					if tid, ok := tcm["id"].(string); !ok || tid == "" {
						st.toolNames[idx] = fmt.Sprintf("toolu_%d_%s", idx, randHex(6))
					} else {
						st.toolNames[idx] = tid
					}
					st.emit("content_block_start", map[string]any{
						"type": "content_block_start", "index": st.blockIdx,
						"content_block": map[string]any{"type": "tool_use", "id": st.toolNames[idx], "name": name, "input": map[string]any{}},
					})
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					st.emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": st.toolBlocks[idx],
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					})
				}
			}
		}
	}
	// 收尾：关块 → message_delta → message_stop
	st.closeOpenBlocks()
	if st.stopReason == "" {
		st.stopReason = "end_turn"
	}
	st.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": st.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": st.outTok},
	})
	st.emit("message_stop", map[string]any{"type": "message_stop"})
}

// openText 打开（或复用）当前 text 块。
func (s *anthStreamState) openText() {
	if s.textOpen {
		return
	}
	s.closeOpenBlocks()
	s.blockIdx++
	s.textOpen = true
	s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// openThinking 打开（或复用）当前 thinking 块。
func (s *anthStreamState) openThinking() {
	if s.thinkOpen {
		return
	}
	s.closeOpenBlocks()
	s.blockIdx++
	s.thinkOpen = true
	s.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

// closeOpenBlocks 关闭所有打开的块（text/thinking；tool 块由索引表管理不在此关）。
func (s *anthStreamState) closeOpenBlocks() {
	if s.thinkOpen {
		s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIdx})
		s.thinkOpen = false
	}
	if s.textOpen {
		s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blockIdx})
		s.textOpen = false
	}
}

// emit 写一帧 Anthropic SSE。
func (s *anthStreamState) emit(event string, data map[string]any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}
