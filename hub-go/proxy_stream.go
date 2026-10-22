package main

// 聚合流式响应：把后端 SSE 聚合成一次完整响应（非流式请求走这里）
// 由 proxy.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ---------- 聚合流式响应 ----------

type toolCallSlot struct {
	id        string
	name      string
	arguments string
}

type collected struct {
	model        string
	finish       string
	usage        map[string]any
	contentParts []string
	reasoning    []string // reasoning_content 片段（Qoder <think> 分流等）
	toolCalls    map[int]*toolCallSlot
	// errMsg / errType 上游在【流内部】报的错（HTTP 已是 200，错误放在
	// data: {"error":{...}} 帧里）。2026-10-21 开源审查补：
	// 此前所有解析器都只认带 choices 的帧，错误帧被静默跳过，于是
	// 「上游报错」在客户端表现为「成功但内容为空」——用户归因为
	// 「模型不输出」，实际是错被吞了。这是本仓库历史上反复出现的事故形态
	// （qoder_stream.go 里 2026-09-25 的记录就是它），但之前只修了 qoder
	// 直连路径，聚合 / Responses 两条路径没修。
	errMsg  string
	errType string
}

func collectStream(r io.Reader) *collected {
	c := &collected{toolCalls: map[int]*toolCallSlot{}}
	sc := bufio.NewScanner(r)
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
		// 上游 HTTP 200 但流里报错：记下来交给调用方改成错误响应，
		// 不要静默丢帧后返回空内容。
		if em, ok := chunk["error"].(map[string]any); ok {
			c.errMsg, c.errType = errorFrameMessage(em)
			break
		}
		if m, ok := chunk["model"].(string); ok && m != "" {
			c.model = m
		}
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			c.usage = u
		}
		for _, ch := range asSlice(chunk["choices"]) {
			cm, _ := ch.(map[string]any)
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				c.finish = fr
			}
			delta, _ := cm["delta"].(map[string]any)
			if ct, ok := delta["content"].(string); ok && ct != "" {
				c.contentParts = append(c.contentParts, ct)
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				c.reasoning = append(c.reasoning, rc)
			}
			for _, tc := range asSlice(delta["tool_calls"]) {
				tcm, _ := tc.(map[string]any)
				idx := intVal(tcm["index"], 0)
				slot := c.toolCalls[idx]
				if slot == nil {
					slot = &toolCallSlot{}
					c.toolCalls[idx] = slot
				}
				if id, ok := tcm["id"].(string); ok && id != "" {
					slot.id = id
				}
				fn, _ := tcm["function"].(map[string]any)
				if nm, ok := fn["name"].(string); ok && nm != "" {
					slot.name = nm
				}
				if args, ok := fn["arguments"].(string); ok {
					slot.arguments += args
				}
			}
		}
	}
	return c
}

func (c *collected) build() map[string]any {
	tcs := []any{}
	if len(c.toolCalls) > 0 {
		idx := make([]int, 0, len(c.toolCalls))
		for k := range c.toolCalls {
			idx = append(idx, k)
		}
		sort.Ints(idx)
		for _, k := range idx {
			v := c.toolCalls[k]
			id := v.id
			if id == "" {
				id = fmt.Sprintf("call_%d", k)
			}
			tcs = append(tcs, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": v.name, "arguments": v.arguments},
			})
		}
		if c.finish == "" {
			c.finish = "tool_calls"
		}
	}
	content := strings.Join(c.contentParts, "")
	msg := map[string]any{"role": "assistant", "content": nil}
	if content != "" {
		msg["content"] = content
	}
	if rc := strings.Join(c.reasoning, ""); rc != "" {
		msg["reasoning_content"] = rc
	}
	if len(tcs) > 0 {
		msg["tool_calls"] = tcs
	}
	finish := c.finish
	if finish == "" {
		finish = "stop"
	}
	usage := c.usage
	if len(usage) == 0 {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	model := c.model
	if model == "" {
		model = "unknown"
	}
	return map[string]any{
		"id":      "chatcmpl-" + randHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   usage,
	}
}

// errorFrameMessage 从 SSE 错误帧的 error 对象里取 (人类可读信息, 类型)。
// 上游几种形态都兼容：{"message":...}、"error":{"message":...}、
// 只有 {"code":..., "msg":...} 的网关包装。
func errorFrameMessage(em map[string]any) (string, string) {
	typ, _ := em["type"].(string)
	if m, ok := em["message"].(string); ok && m != "" {
		return m, typ
	}
	if m, ok := em["msg"].(string); ok && m != "" {
		return m, typ
	}
	if d, ok := em["details"].(string); ok && d != "" {
		return d, typ
	}
	if c, ok := em["code"]; ok && c != nil {
		return "upstream error code " + fmt.Sprint(c), typ
	}
	return "upstream returned an error frame", typ
}
