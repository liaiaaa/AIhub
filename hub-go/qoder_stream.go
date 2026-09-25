package main

// 流改写与上游调用：SSE 解包、<think> 分流、聚合
// 由 proxy_qoder.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------- 思考/正文分流（<think> → reasoning_content） ----------

// qoderThinkState 流式分流的会话状态（每个请求一个）。
type qoderThinkState struct {
	inThink bool
	buf     string // 跨帧缓冲：可能一个 chunk 里只有 "<thi"，要等下一帧才能判定
}

// qoderMaxTagLen 标签最大长度（"<think>"/"</think>" 均 9 字节内）。
const qoderMaxTagLen = 16

func newQoderThinkState() *qoderThinkState { return &qoderThinkState{} }

// split 把一帧的 content 文本拆成 (正文部分, 思考部分)。
// 标签可能跨帧：不够长无法判定的尾部进 buf，等下一帧。
// 流结束时调用 flush() 吐出残留缓冲。
func (s *qoderThinkState) split(text string) (out, think string) {
	s.buf += text
	for {
		if s.inThink {
			end := strings.Index(s.buf, "</think>")
			if end < 0 {
				// 可能 "</think" 被截断：保留可能构成尾标签的尾部
				keep := qoderSafeKeep(s.buf, "</think>")
				thinkOut := s.buf[:len(s.buf)-keep]
				s.buf = s.buf[len(s.buf)-keep:]
				return out, thinkOut
			}
			think += s.buf[:end]
			s.buf = s.buf[end+len("</think>"):]
			s.inThink = false
			continue
		}
		start := strings.Index(s.buf, "<think>")
		if start < 0 {
			// 可能 "<think" 被截断：保留可能构成头标签的尾部
			keep := qoderSafeKeep(s.buf, "<think>")
			outPart := s.buf[:len(s.buf)-keep]
			s.buf = s.buf[len(s.buf)-keep:]
			return out + outPart, think
		}
		out += s.buf[:start]
		s.buf = s.buf[start+len("<think>"):]
		s.inThink = true
	}
}

// qoderSafeKeep 返回 buf 尾部可能构成 tag 前缀的最长长度。
func qoderSafeKeep(buf, tag string) int {
	max := len(tag) - 1
	if max > len(buf) {
		max = len(buf)
	}
	for k := max; k > 0; k-- {
		if strings.HasPrefix(tag, buf[len(buf)-k:]) {
			return k
		}
	}
	return 0
}

// flush 流结束时吐出残留缓冲（未闭合的 <think> 内容归入思考）。
func (s *qoderThinkState) flush() (out, think string) {
	rest := s.buf
	s.buf = ""
	if s.inThink {
		return "", rest
	}
	return rest, ""
}

// qoderRewriteChunk 对单个内层 OpenAI chunk 做 think→reasoning_content 转写。
// 返回转写后的 JSON 文本；若该帧无变化则返回原文本。
func qoderRewriteChunk(raw string, st *qoderThinkState, final bool) string {
	var chunk map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&chunk) != nil {
		return raw
	}
	changed := false
	for _, ch := range asSlice(chunk["choices"]) {
		cm, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := cm["delta"].(map[string]any)
		if !ok {
			continue
		}
		ct, ok := delta["content"].(string)
		if !ok {
			continue
		}
		var out, think string
		if final {
			// 收尾：把缓冲全部吐出
			out, think = st.split(ct)
			o2, t2 := st.flush()
			out += o2
			think += t2
		} else {
			out, think = st.split(ct)
		}
		// 关键：即使 out/think 都为空，只要输入非空就说明本帧内容被状态机
		// 消费（如整帧恰好是 "<think>" 标签），必须重写帧清空 content，
		// 否则标签原样透传，客户端会把标签当正文显示。
		if ct != "" {
			changed = true
		}
		// reasoning_content 追加（上游本无此字段；一帧可能同时含两者）
		if think != "" {
			prev, _ := delta["reasoning_content"].(string)
			delta["reasoning_content"] = prev + think
			changed = true
		}
		if out != "" {
			delta["content"] = out
		} else {
			delete(delta, "content") // 该帧纯标签/纯思考：正文置空
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return raw
	}
	return string(b)
}

// qoderRewriteStreamForThinking 对整段内层 SSE 做逐帧 think→reasoning_content 转写
// （非流式聚合路径用）。
func qoderRewriteStreamForThinking(sse string) string {
	st := newQoderThinkState()
	var out strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") || trimmed == "data: [DONE]" {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		raw := strings.TrimSpace(trimmed[5:])
		out.WriteString("data: " + qoderRewriteChunk(raw, st, false) + "\n")
	}
	// 残留缓冲并入最后一个 content 帧
	if rest, think := st.flush(); rest != "" || think != "" {
		out.WriteString("data: " + jsonMust(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": rest, "reasoning_content": think},
		}}}) + "\n")
	}
	return out.String()
}

func jsonMust(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// qoderNormalizeMessages：developer → system（对齐其他代理的行为）。
func qoderNormalizeMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		if role, _ := mm["role"].(string); role == "developer" {
			nm := cloneMap(mm)
			nm["role"] = "system"
			out = append(out, nm)
			changed = true
			continue
		}
		out = append(out, m)
	}
	if !changed {
		return msgs
	}
	return out
}

// openInfer 签名并调用上游，带自愈重试：
//   - 网络错误 / 5xx：等 700ms 重试（最多 3 次尝试）
//   - 401：重读桌面端凭据后重签重试（桌面端平时会自动刷新 auth.v1.dat）
//
// 返回的 resp 由调用方关闭。
func (p *QoderProxy) openInfer(ctx context.Context, payload []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(700 * time.Millisecond)
		}
		headers, serr := p.auth.Sign(p.inferURL, string(payload))
		if serr != nil {
			return nil, serr
		}
		req, rerr := http.NewRequestWithContext(ctx, "POST", p.inferURL, bytes.NewReader(payload))
		if rerr != nil {
			return nil, rerr
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err = p.client.Do(req)
		if err != nil {
			if attempt == 2 {
				return nil, err
			}
			continue // 网络抖动重试
		}
		if resp.StatusCode == http.StatusUnauthorized {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			resp = nil
			if rerr := p.auth.Reload(); rerr != nil && attempt == 2 {
				return nil, fmt.Errorf("凭据已过期且重读失败：%v", rerr)
			}
			continue // 换新凭据重试
		}
		if resp.StatusCode >= 500 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			resp = nil
			if attempt == 2 {
				return nil, fmt.Errorf("上游持续 5xx")
			}
			continue
		}
		return resp, nil
	}
	if resp == nil && err == nil {
		err = fmt.Errorf("上游调用失败")
	}
	return resp, err
}

// firstFrameUpstreamError 检查首帧是否为上游错误帧（如配额瞬时抽风的
// provider_error，HTTP 状态仍是 200，只能从内容识别）。
func firstFrameUpstreamError(head []byte) bool {
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		var probe struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Choices []any  `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &probe) != nil {
			return false
		}
		// 上游错误帧：有 code/message、没有 choices；正常帧要么是包装帧（有 body），要么有 choices
		if probe.Code != "" && probe.Message != "" && probe.Choices == nil {
			return true
		}
		return false
	}
	return false
}

// openInferValidated 在 openInfer 之上多做一层首帧错误检测：
// HTTP 200 但首帧是 provider_error（如配额瞬时抽风）时整体重试一次。
// Peek(1) 只等到上游第一个字节到达（通常 ~300ms），不会人为增加首包延迟。
//
// ⚠️ 关键：bufio.Peek **不消费**缓冲区，Peek 过的字节仍留在 br 里——
// 所以这里直接把 br 包回 resp.Body，绝不能把预读的 head 再拼回流前面
// （否则已缓冲字节会被读两遍，客户端 JSON.parse 报
// "Unexpected non-whitespace character after JSON"）。
func (p *QoderProxy) openInferValidated(ctx context.Context, payload []byte, log *FileLogger, rid string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := p.openInfer(ctx, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return resp, nil // 非 200 交给上层报错
		}
		br := bufio.NewReaderSize(resp.Body, 16*1024)
		perr := error(nil)
		if _, perr = br.Peek(1); perr != nil && perr != io.EOF {
			// 首字节都读不出来（网络/上下文异常）：原样交回，上层按流错误处理
			resp.Body = &qBodyReader{r: br, c: resp.Body}
			return resp, nil
		}
		head, _ := br.Peek(br.Buffered()) // 只用于错误帧探测，不消费
		if attempt == 0 && len(head) > 0 && firstFrameUpstreamError(head) {
			resp.Body.Close()
			if log != nil {
				log.Log("[%s] ⚠ 上游首帧 provider_error，700ms 后重试", rid)
			}
			time.Sleep(700 * time.Millisecond)
			continue
		}
		resp.Body = &qBodyReader{r: br, c: resp.Body}
		return resp, nil
	}
}

// qBodyReader 包装重缓冲后的响应流（br 内保有 Peek 过但未消费的字节），
// Close 透传给原始 body。
type qBodyReader struct {
	r io.Reader
	c io.Closer
}

func (b *qBodyReader) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *qBodyReader) Close() error               { return b.c.Close() }

// qoderUnwrapFrames 把上游包装帧流转成内层 OpenAI SSE 文本。
// 返回首帧错误信息（空串表示正常）；聚合模式整体缓冲使用。
func qoderUnwrapFrames(r io.Reader, out *bytes.Buffer) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	firstErr := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[5:])
		var wrapper qoderSSEWrapper
		if json.Unmarshal([]byte(raw), &wrapper) != nil || wrapper.Body == "" {
			// 不是包装帧：可能是裸错误帧，也可能是裸 OpenAI chunk
			var probe map[string]any
			if json.Unmarshal([]byte(raw), &probe) == nil {
				if c, _ := probe["code"].(string); c != "" {
					if _, hasMsg := probe["message"]; hasMsg {
						if _, hasChoices := probe["choices"]; !hasChoices {
							if firstErr == "" {
								firstErr = raw
							}
							continue
						}
					}
				}
			}
			fmt.Fprintf(out, "data: %s\n\n", raw)
			continue
		}
		if wrapper.Body == "[DONE]" {
			out.WriteString("data: [DONE]\n\n")
			continue
		}
		fmt.Fprintf(out, "data: %s\n\n", wrapper.Body)
	}
	return firstErr
}

func (p *QoderProxy) streamToClient(w http.ResponseWriter, r *http.Request, payload []byte, t0 time.Time, rid string) {
	thinkFrames := 0 // 诊断：含 reasoning_content 的帧数
	resp, err := p.openInferValidated(r.Context(), payload, p.log, rid)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		sseError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		sseError(w, resp.StatusCode, truncate(string(b), 500))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// 边解包边转发：包装帧 → 内层 OpenAI chunk，逐帧 flush（首块已由
	// openInferValidated 拼回 body 最前，不会丢帧）。
	// 思考分流：<think> 标签内容 → reasoning_content（客户端渲染成折叠思考框）。
	st := newQoderThinkState()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") || !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[5:])
		var wrapper qoderSSEWrapper
		if json.Unmarshal([]byte(raw), &wrapper) == nil && wrapper.Body != "" {
			raw = wrapper.Body
		}
		if raw == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
		} else {
			rw := qoderRewriteChunk(raw, st, false)
			if strings.Contains(rw, `"reasoning_content"`) {
				thinkFrames++
			}
			fmt.Fprintf(w, "data: %s\n\n", rw)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 流结束：吐出分流缓冲的残留（缓冲里的内容已无法归帧，直接并入正文）
	if out, think := st.flush(); out != "" || think != "" {
		tail := map[string]any{"choices": []any{map[string]any{
			"index": 0, "finish_reason": nil,
			"delta": func() map[string]any {
				d := map[string]any{}
				if think != "" {
					d["reasoning_content"] = think
				}
				if out != "" {
					d["content"] = out
				}
				return d
			}(),
		}}}
		if b, err := json.Marshal(tail); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | stream | think=%d帧", rid, time.Since(t0).Seconds(), thinkFrames)
	}
}

func (p *QoderProxy) aggregateToClient(w http.ResponseWriter, r *http.Request, payload []byte, model string, t0 time.Time, rid string) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	resp, err := p.openInferValidated(ctx, payload, p.log, rid)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"},
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		var detail any
		if json.Unmarshal(b, &detail) == nil {
			writeJSON(w, resp.StatusCode, detail)
		} else {
			writeJSON(w, resp.StatusCode, map[string]any{
				"error": map[string]any{"message": string(b), "type": "upstream_error", "code": resp.StatusCode},
			})
		}
		return
	}

	// 整体解包成内层 SSE，再复用 hub 的聚合器（tool_calls/usage/finish 全支持）。
	// 思考分流：<think> 内容 → reasoning_content（聚合时合并进 message）。
	var inner bytes.Buffer
	if ferr := qoderUnwrapFrames(resp.Body, &inner); ferr != "" {
		// 首帧即上游错误（重试后仍失败）：透传给客户端
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": truncate(ferr, 500), "type": "upstream_error"},
		})
		return
	}
	innerStr := qoderRewriteStreamForThinking(inner.String())
	col := collectStream(strings.NewReader(innerStr))
	if col.model != "" && col.model != "unknown" {
		model = col.model
	}
	elapsed := time.Since(t0)
	tokens := "?"
	if col.usage != nil {
		if v, ok := col.usage["total_tokens"].(json.Number); ok {
			tokens = v.String()
		}
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | finish=%s | tokens=%s | think=%d段", rid, elapsed.Seconds(), col.finish, tokens, len(col.reasoning))
	}
	res := col.build()
	res["model"] = model
	writeJSON(w, http.StatusOK, res)
}

