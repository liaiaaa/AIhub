package main

// 流改写与上游调用：SSE 解包、<think> 分流、聚合
// 由 proxy_qoder.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 思考/正文分流（<think>/<thinking> → reasoning_content） ----------

// qoderThinkTagPairs 支持的思考标签对（官方 CLI 两种都出现；<think> 是
// <thinking> 的前缀，匹配时同位置须取更长者）。
var qoderThinkTagPairs = [][2]string{
	{"<think>", "</think>"},
	{"<thinking>", "</thinking>"},
}

// qoderThinkState 流式分流的会话状态（每个请求一个）。
type qoderThinkState struct {
	inThink  bool
	closeTag string // 与开标签配对的闭合标签
	buf      string // 跨帧缓冲：可能一个 chunk 里只有 "<thi"，要等下一帧才能判定
}

// qoderMaxTagLen 标签最大长度（"<thinking>"/"</thinking>" 均 14 字节内）。
const qoderMaxTagLen = 16

func newQoderThinkState() *qoderThinkState {
	return &qoderThinkState{closeTag: qoderThinkTagPairs[0][1]}
}

// qoderEarliestOpen 找 buf 中最早出现的开标签。
// 同位置命中时取更长的开标签（<think> 是 <thinking> 的前缀，须整体匹配）。
func qoderEarliestOpen(buf string) (pos int, open, close string, found bool) {
	pos, open, close, found = -1, "", "", false
	for _, pair := range qoderThinkTagPairs {
		i := strings.Index(buf, pair[0])
		if i < 0 {
			continue
		}
		if !found || i < pos || (i == pos && len(pair[0]) > len(open)) {
			pos, open, close, found = i, pair[0], pair[1], true
		}
	}
	return
}

// split 把一帧的 content 文本拆成 (正文部分, 思考部分)。
// 标签可能跨帧：不够长无法判定的尾部进 buf，等下一帧。
// 流结束时调用 flush() 吐出残留缓冲。
func (s *qoderThinkState) split(text string) (out, think string) {
	s.buf += text
	for {
		if s.inThink {
			end := strings.Index(s.buf, s.closeTag)
			if end < 0 {
				// 可能闭合标签被截断：保留可能构成尾标签的尾部
				keep := qoderSafeKeep(s.buf, s.closeTag)
				thinkOut := s.buf[:len(s.buf)-keep]
				s.buf = s.buf[len(s.buf)-keep:]
				return out, thinkOut
			}
			think += s.buf[:end]
			s.buf = s.buf[end+len(s.closeTag):]
			s.inThink = false
			continue
		}
		start, open, close, found := qoderEarliestOpen(s.buf)
		if !found {
			// 可能开标签被截断：保留可能构成头标签的尾部
			keep := qoderSafeKeepAny(s.buf)
			outPart := s.buf[:len(s.buf)-keep]
			s.buf = s.buf[len(s.buf)-keep:]
			return out + outPart, think
		}
		out += s.buf[:start]
		s.buf = s.buf[start+len(open):]
		s.inThink = true
		s.closeTag = close
	}
}

// qoderSafeKeepAny 返回 buf 尾部可能构成任一标签前缀的最长长度。
func qoderSafeKeepAny(buf string) int {
	best := 0
	for _, pair := range qoderThinkTagPairs {
		for _, tag := range pair {
			if k := qoderSafeKeep(buf, tag); k > best {
				best = k
			}
		}
	}
	return best
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
		if trimmed == "data: [DONE]" {
			// 残留缓冲必须在 [DONE] 之前并入：聚合器收到 DONE 即停止
			if rest, think := st.flush(); rest != "" || think != "" {
				out.WriteString("data: " + jsonMust(map[string]any{"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": rest, "reasoning_content": think},
				}}}) + "\n")
			}
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		raw := strings.TrimSpace(trimmed[5:])
		out.WriteString("data: " + qoderRewriteChunk(raw, st, false) + "\n")
	}
	// 上游没发 [DONE] 就断流时兜底（flush 后缓冲为空，重复调用无副作用）
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

// qoderNormalizeMessages：developer → system；剔除空内容 system 消息。
//
// 后者有真实事故（2026-09-26 二分定位）：DSH 在会话中途切换模型时插入
// {"role":"system","content":[]}，Qoder 网关对它返回 provider_error
// （Error in upstream response），且重试无效——整条会话从此打不通。
func qoderNormalizeMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "developer" {
			nm := cloneMap(mm)
			nm["role"] = "system"
			out = append(out, nm)
			changed = true
			continue
		}
		if role == "system" && qoderMsgContentEmpty(mm["content"]) {
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

// qoderMsgContentEmpty 判断消息 content 是否实际为空（""/空数组/全空 text 段）。
func qoderMsgContentEmpty(v any) bool {
	switch c := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(c) == ""
	case []any:
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				return false // 非文本段（图等）：视为非空
			}
			if t, ok := pm["text"].(string); !ok || strings.TrimSpace(t) != "" {
				return false
			}
		}
		return true
	}
	return false
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
//
// 兼容两种形态（2026-09-26 教训：此前只认裸错误帧，包装形态
// {"headers":...,"body":"{\"code\":\"provider_error\"...}"} 被漏判，
// 导致「首帧错误自动重试」从未在包装路径上生效——用户看到的每次
// provider_error 失败其实都没走到重试）：
//
//	裸帧:   {"code":"provider_error","message":"Error in upstream response"}
//	包装帧: {"headers":{...},"body":"{\"code\":\"provider_error\",...}"}
//
// 从第一个 data 行向后扫描，直到出现决定性帧（错误帧 → true；正常
// choices 帧 → false）；不可解析的行（心跳等）跳过。
func firstFrameUpstreamError(head []byte) bool {
	_, ok := decisiveFirstFrame(head)
	return ok
}

// decisiveFirstFrame 返回头几个字节里首个「决定性」帧的文本：错误帧
// （有 code/message、无 choices）→ (帧文本, true)；正常内容帧或流已正常
// 结束 → ("", false)；不可解析的行（心跳等）跳过。排队帧也是错误帧的
// 子集（见 qoder_queue.go），调用方拿到帧文本后可用 parseQueueFrame 细分。
func decisiveFirstFrame(head []byte) (string, bool) {
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "" {
			continue
		}
		var wrapper qoderSSEWrapper
		if json.Unmarshal([]byte(data), &wrapper) == nil && wrapper.Body != "" {
			data = strings.TrimSpace(wrapper.Body)
		}
		if data == "" || data == "[DONE]" {
			return "", false
		}
		var probe struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Choices []any  `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &probe) != nil {
			continue // 非JSON/心跳类行：看下一行
		}
		// 上游错误帧：有 code/message、没有 choices；正常帧要么是包装帧（有 body），要么有 choices
		if probe.Code != "" && probe.Message != "" && probe.Choices == nil {
			return data, true
		}
		if probe.Choices != nil {
			return "", false // 正常内容帧先行，不算错误
		}
	}
	return "", false
}

// openInferValidated 在 openInfer 之上多做两层首帧错误处理：
//   - 首帧是普通上游错误帧（如配额瞬时抽风的 provider_error，HTTP 状态
//     仍是 200）：整体重试最多 2 次（700ms 间隔；2026-09-26 实测单次重试
//     不够，且此前包装错误帧根本没触发过重试，见 firstFrameUpstreamError 注释）。
//   - 首帧是排队帧（业务码 10605，见 qoder_queue.go）：按网关建议间隔
//     retryAfterSeconds 真实等待后重试，总预算默认 240s
//     （QODER_QUEUE_MAX_WAIT_SECS 可调，0 关闭）；预算耗尽返回 429 语义的
//     qoderQueueError。HTTP 403 + 10605 排队体的形态同样兜底处理。
//     每次等待前回调 onQueueWait（流式路径借此发心跳保活）。
//
// Peek(1) 只等到上游第一个字节到达（通常 ~300ms），不会人为增加首包延迟。
//
// ⚠️ 关键：bufio.Peek **不消费**缓冲区，Peek 过的字节仍留在 br 里——
// 所以这里直接把 br 包回 resp.Body，绝不能把预读的 head 再拼回流前面
// （否则已缓冲字节会被读两遍，客户端 JSON.parse 报
// "Unexpected non-whitespace character after JSON"）。
func (p *QoderProxy) openInferValidated(ctx context.Context, payload []byte, log *FileLogger, rid string, onQueueWait func(waitSecs, totalSecs int)) (*http.Response, error) {
	budget := qoderQueueWaitBudget()
	var queueWait time.Duration
	providerErrs := 0
	// queueRetry 等待后继续重试返回 true；放弃（预算耗尽/服务不可用）返回
	// false；等待被 ctx 打断返回 err。
	queueRetry := func(qi qoderQueueInfo) (bool, error) {
		wait, giveup := qoderQueueNextWait(ctx, qi, queueWait, budget)
		if giveup {
			return false, nil
		}
		if log != nil {
			log.Log("[%s] ⚠ 上游排队(%s)：队列 %d 人，等 %ds 后重试（已等 %ds/预算 %ds）",
				rid, qi.Code, qi.QueueCount, int(wait.Seconds()), int(queueWait.Seconds()), int(budget.Seconds()))
		}
		if onQueueWait != nil {
			onQueueWait(int(wait.Seconds()), int((queueWait + wait).Seconds()))
		}
		if err := qoderSleepCtx(ctx, wait); err != nil {
			return false, err
		}
		queueWait += wait
		return true, nil
	}
	for {
		resp, err := p.openInfer(ctx, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			// 观测的排队形态是 HTTP 200 + 错误帧，但网关不同入口可能直接回
			// HTTP 403 + 10605 排队体，这里兜底同样按队列重试。
			if resp.StatusCode == http.StatusForbidden {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if qi, isQueue := parseQueueFrame(string(b)); isQueue {
					ok, qerr := queueRetry(qi)
					if qerr != nil {
						return nil, qerr
					}
					if ok {
						continue
					}
					return nil, &qoderQueueError{Info: qi, Waited: queueWait}
				}
				resp.Body = io.NopCloser(bytes.NewReader(b)) // 非排队 403：原样交回上层
			}
			return resp, nil
		}
		br := bufio.NewReaderSize(resp.Body, 16*1024)
		perr := error(nil)
		if _, perr = br.Peek(1); perr != nil && perr != io.EOF {
			// 首字节都读不出来（网络/上下文异常）：原样交回，上层按流错误处理
			resp.Body = &qBodyReader{r: br, c: resp.Body}
			return resp, nil
		}
		head, _ := br.Peek(br.Buffered()) // 只用于错误帧探测，不消费
		if data, ok := decisiveFirstFrame(head); ok {
			if qi, isQueue := parseQueueFrame(data); isQueue {
				resp.Body.Close()
				ok2, qerr := queueRetry(qi)
				if qerr != nil {
					return nil, qerr
				}
				if ok2 {
					continue
				}
				return nil, &qoderQueueError{Info: qi, Waited: queueWait}
			}
			if providerErrs < 2 {
				resp.Body.Close()
				if log != nil {
					log.Log("[%s] ⚠ 上游首帧错误(%s)，重试 %d/2", rid, truncate(data, 120), providerErrs+1)
				}
				if providerErrs == 0 {
					qoderDumpFailedPayload(rid, payload, log)
				}
				time.Sleep(700 * time.Millisecond)
				providerErrs++
				continue
			}
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

// ---------- 大载荷 provider_error 的自动取证与提示（2026-10-07） ----------
//
// 观测：dsh 会话临近上下文上限时（msgs≈1000），两个载荷在 qfmodel 上首帧
// provider_error 确定性失败（各重试 3 次同样错误），同时段小载荷全部正常。
// 事后合成探针（300k/633k/955k prompt tokens；纯文本与工具历史两种形态、
// stream=true）在同一账号同一模型上全部成功——说明不是一般性的容量或
// 形态上限，更可能是该载荷的具体内容触发上游模型后端异常。真凶载荷当时
// 未留存（dump 开关没开），因此补上三件事：
//   1) ▶ REQUEST 日志行永久带上 payload 体积（见 qoder_proxy.go handleChat）；
//   2) 首帧 provider_error 重试前，大载荷自动 dump（数据目录
//      qoder-dump-<rid>-upstream.json，即发往网关的最终信封，可直接重放）；
//   3) 客户端可见错误追加 [hub] 提示（见 qoderBigPayloadHint）。

// qoderBigPayloadBytes 视为「大载荷」的门槛（仅用于取证 dump 与提示措辞）。
const qoderBigPayloadBytes = 512 << 10

// qoderDumpFailedPayload 首帧 provider_error 且载荷较大时，把发往网关的
// 最终信封原样落盘供重放定位；小载荷/成功路径零开销。
func qoderDumpFailedPayload(rid string, payload []byte, log *FileLogger) {
	if len(payload) < qoderBigPayloadBytes || log == nil {
		return
	}
	dumpPath := dataPath("qoder-dump-" + rid + "-upstream.json")
	if err := os.WriteFile(dumpPath, payload, 0644); err == nil {
		log.Log("[%s] ⚠ 已 dump 失败载荷: %s (%dKB)，可重放定位", rid, dumpPath, len(payload)/1024)
		qoderPruneDumps(log)
	}
}

// qoderDumpKeep 最多保留几份 qoder dump。dump 单份可达 1MB+（整个请求信封），
// 无上限的话会一直涨（2026-10-19 清理时已积 12 份 26MB）。留最近 5 份足够
// 对比定位，再老的没有回看价值。
const qoderDumpKeep = 5

// qoderPruneDumps dump 落盘后清掉超出保留数的旧文件（按修改时间从旧删起）。
// 只动 qoder-dump-*.json 这一种前缀，碰不到数据根里其它东西；
// 清理失败只记日志——取证失败不该影响正常请求路径。
func qoderPruneDumps(log *FileLogger) {
	matches, err := filepath.Glob(dataPath("qoder-dump-*.json"))
	if err != nil || len(matches) <= qoderDumpKeep {
		return
	}
	type fileInfo struct {
		path string
		mod  time.Time
	}
	files := make([]fileInfo, 0, len(matches))
	for _, p := range matches {
		if st, err := os.Stat(p); err == nil {
			files = append(files, fileInfo{p, st.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) }) // 新→旧
	removed := 0
	for _, f := range files[qoderDumpKeep:] {
		if err := os.Remove(f.path); err == nil {
			removed++
		}
	}
	if removed > 0 && log != nil {
		log.Log("已清理 %d 份过期 qoder dump（保留最近 %d 份）", removed, qoderDumpKeep)
	}
}

// qoderBigPayloadHint 大载荷上游错误的客户端提示（拼在错误 message 尾部）。
func qoderBigPayloadHint(payloadLen int) string {
	if payloadLen < qoderBigPayloadBytes {
		return ""
	}
	return fmt.Sprintf("\n\n[hub] 本请求载荷 %.1fMB：上游模型后端拒绝该载荷（重试无效，同账号同时段其他请求正常）。建议在 dsh 压缩上下文或新开会话后重试；失败载荷已自动 dump 供定位。", float64(payloadLen)/(1<<20))
}

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

// upstreamErrorFrame 识别网关的「业务错误帧」并抽出可读信息。
//
// 形态（HTTP 200，但没有 choices，只有 code+message）：
//
//	{"code":"400","message":"[FAIL]node:oa_qwen-plus-main msg:Execution failed: ..."}
//
// 返回 (错误文案, true) 表示这是错误帧。正常 OpenAI chunk（有 choices）不受影响。
func upstreamErrorFrame(raw string) (string, bool) {
	if raw == "" || raw == "[DONE]" || !strings.HasPrefix(raw, "{") {
		return "", false
	}
	var probe map[string]any
	if json.Unmarshal([]byte(raw), &probe) != nil {
		return "", false
	}
	// 正常数据帧带 choices，直接放行
	if _, hasChoices := probe["choices"]; hasChoices {
		return "", false
	}
	// 已是标准 OpenAI error 形态，无需再包装
	if _, hasErr := probe["error"]; hasErr {
		return "", false
	}
	// 排队帧（10605）：改写成 429 排队语义。照搬原始 code（外层是 403）会把
	// 客户端误导成「API 密钥无效」（2026-09-26 实测）。
	if qi, ok := parseQueueFrame(raw); ok {
		return (&qoderQueueError{Info: qi}).Error(), true
	}
	msg, _ := probe["message"].(string)
	code := ""
	switch c := probe["code"].(type) {
	case string:
		code = c
	case json.Number:
		code = c.String()
	case float64:
		code = strconv.FormatFloat(c, 'f', -1, 64)
	}
	if msg == "" && code == "" {
		return "", false
	}
	if code != "" && msg != "" {
		return fmt.Sprintf("上游网关错误 %s: %s", code, msg), true
	}
	if msg != "" {
		return msg, true
	}
	return "上游网关错误 " + code, true
}

// mustJSON 序列化失败时返回一个合法的错误 JSON（不会返回半截字符串）。
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":{"message":"marshal failed","type":"internal_error"}}`
	}
	return string(b)
}

// qoderWriteTailFrame 把分流残留作为最后一个 delta 帧写出。
// 必须在 data: [DONE] 之前调用——客户端收到 DONE 即停止解析。
func qoderWriteTailFrame(w http.ResponseWriter, flusher http.Flusher, out, think string) {
	if out == "" && think == "" {
		return
	}
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

// streamToClient 真流式转发。
//
// 返回值 retryDegraded=true 表示「在写 SSE 头之前就发现上游不可用」，
// 调用方应改用 degradedStreamToClient 重试一次（2026-10-19 加）。
// 一旦已开始向客户端写帧，就再没有回退机会，此时一律返回 false。
func (p *QoderProxy) streamToClient(w http.ResponseWriter, r *http.Request, payload []byte, t0 time.Time, rid string) (retryDegraded bool) {
	thinkFrames := 0 // 诊断：含 reasoning_content 的帧数
	// 临时诊断（QODER_DIAG_STREAM=1）：累计 reasoning / content / tool_calls 的实际内容，
	// 用于定位「客户端到底收到了什么」。默认关闭，不影响正常路径。
	var diagReason, diagContent strings.Builder
	diagTools := 0
	diagOn := os.Getenv("QODER_DIAG_STREAM") == "1"
	// SSE 头在首次输出前才写；排队等待期间先发心跳，保证客户端连接活跃
	// （否则整个等待期零字节，客户端的静默超时会掐掉一个本来能成的请求）。
	flusher, _ := w.(http.Flusher)
	headersSent := false
	ensureHeaders := func() {
		if headersSent {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		headersSent = true
	}
	writeErrEvent := func(code int, msg string) {
		ensureHeaders()
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": code}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 排队心跳：空 delta 帧与网关自带的空 usage 帧同形（DSH 已验证容忍），
	// 间隔 = 网关建议重试间隔（约 30s），远小于常见静默超时。
	onWait := func(waitSecs, totalSecs int) {
		ensureHeaders()
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{
			"index": 0, "finish_reason": nil, "delta": map[string]any{},
		}}}))
		if flusher != nil {
			flusher.Flush()
		}
	}
	resp, err := p.openInferValidated(r.Context(), payload, p.log, rid, onWait)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		var qe *qoderQueueError
		if errors.As(err, &qe) {
			writeErrEvent(qe.HTTPStatus(), qe.Error())
		} else {
			// 尚未写 SSE 头 → 允许调用方降级重试一次
			if !headersSent {
				return true
			}
			writeErrEvent(http.StatusBadGateway, err.Error())
		}
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		if qi, ok := parseQueueFrame(string(b)); ok {
			writeErrEvent(http.StatusTooManyRequests, (&qoderQueueError{Info: qi}).Error())
			return false
		}
		// 上游非 200 且尚未写 SSE 头：先不向客户端吐错误，交给调用方降级重试
		if !headersSent {
			return true
		}
		writeErrEvent(resp.StatusCode, truncate(string(b), 500))
		return false
	}

	ensureHeaders()

	// 边解包边转发：包装帧 → 内层 OpenAI chunk，逐帧 flush（首块已由
	// openInferValidated 拼回 body 最前，不会丢帧）。
	// 思考分流：<think> 标签内容 → reasoning_content（客户端渲染成折叠思考框）。
	st := newQoderThinkState()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	// 上游业务错误计数：网关会在 HTTP 200 的流里塞 {"code":"400","message":...}
	// 这种裸错误帧。OpenAI 客户端只认 choices / error，裸错误帧会被静默忽略，
	// 客户端最终表现为「模型没有任何输出」，完全看不到真实原因（2026-09-25 实测）。
	// 这里把它转写成标准 OpenAI error 事件，保证错误一定可见。
	upstreamErrs := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") || !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[5:])
		// 诊断（QODER_DIAG_STREAM=1）：先于一切转换打印网关原始帧全文。
		// 2026-09-26 事故：DSH 大载荷重放必现 provider_error，错误详情此前
		// 只被 upstreamErrorFrame 抽成 code+message，包装帧 headers 全部丢失；
		// 此开关用于拿回完整原始错误帧（requestId/node/额度信息都在里面）。
		if diagOn {
			p.log.Log("[%s] ▶ SSE原帧 | %s", rid, raw)
		}
		var wrapper qoderSSEWrapper
		if json.Unmarshal([]byte(raw), &wrapper) == nil && wrapper.Body != "" {
			raw = wrapper.Body
		}
		// 网关裸错误帧 → 标准 OpenAI error（否则客户端会静默忽略）
		if errMsg, ok := upstreamErrorFrame(raw); ok {
			upstreamErrs++
			if p.log != nil {
				p.log.Log("[%s] ✗ 上游错误帧 #%d | %s", rid, upstreamErrs, truncate(errMsg, 300))
			}
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
				"error": map[string]any{
					"message": errMsg + qoderBigPayloadHint(len(payload)),
					"type":    "upstream_error",
					"code":    "upstream_error",
				},
			}))
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if raw == "[DONE]" {
			// 残留缓冲必须在 [DONE] 之前吐出：DONE 之后的帧会被客户端忽略
			// （旧实现把残留写在 DONE 之后，尾部分片永远丢失）。
			if out, think := st.flush(); out != "" || think != "" {
				qoderWriteTailFrame(w, flusher, out, think)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
		} else {
			rw := qoderRewriteChunk(raw, st, false)
			if strings.Contains(rw, `"reasoning_content"`) {
				thinkFrames++
			}
			// 临时诊断：提取实际内容
			if diagOn {
				var dm map[string]any
				if json.Unmarshal([]byte(rw), &dm) == nil {
					for _, ch := range asSlice(dm["choices"]) {
						cm, _ := ch.(map[string]any)
						d, _ := cm["delta"].(map[string]any)
						if s, _ := d["reasoning_content"].(string); s != "" {
							diagReason.WriteString(s)
						}
						if s, _ := d["content"].(string); s != "" {
							diagContent.WriteString(s)
						}
						if d["tool_calls"] != nil {
							diagTools++
						}
					}
				}
			}
			fmt.Fprintf(w, "data: %s\n\n", rw)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 流结束：吐出分流缓冲的残留（正常路径已在 [DONE] 前吐过，这里兜底
	// 处理上游没发 [DONE] 就断流的情况；flush 后缓冲为空，重复调用无副作用）。
	if out, think := st.flush(); out != "" || think != "" {
		qoderWriteTailFrame(w, flusher, out, think)
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | stream | think=%d帧", rid, time.Since(t0).Seconds(), thinkFrames)
		// 临时诊断：把客户端实际收到的三类内容落盘，便于核对
		if diagOn {
			p.log.Log("[%s] ◀ DIAG | reasoning=%d字 content=%d字 tool_call帧=%d",
				rid, diagReason.Len(), diagContent.Len(), diagTools)
			if diagReason.Len() > 0 {
				p.log.Log("[%s] ◀ DIAG reasoning 全文:\n%s", rid, truncate(diagReason.String(), 4000))
			}
			p.log.Log("[%s] ◀ DIAG content 全文:\n%s", rid, truncate(diagContent.String(), 2000))
		}
	}
	return false
}

func (p *QoderProxy) aggregateToClient(w http.ResponseWriter, r *http.Request, payload []byte, model string, t0 time.Time, rid string) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	// 非流式 JSON 无法发心跳，靠 300s ctx 兜底（预算 240s < 300s 常见静默超时）
	resp, err := p.openInferValidated(ctx, payload, p.log, rid, nil)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		status := http.StatusBadGateway
		msg := "upstream error: " + err.Error()
		var qe *qoderQueueError
		if errors.As(err, &qe) {
			status = qe.HTTPStatus()
			msg = qe.Error()
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"message": msg, "type": "upstream_error"},
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		if qi, ok := parseQueueFrame(string(b)); ok {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{"message": (&qoderQueueError{Info: qi}).Error(), "type": "upstream_error"},
			})
			return
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
		// 首帧即上游错误（重试后仍失败）：转写成可读文案透传（裸 JSON 里
		// 带外层 403 会再次误导客户端分类）
		msg, isErr := upstreamErrorFrame(ferr)
		if !isErr {
			msg = truncate(ferr, 500)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": truncate(msg, 500) + qoderBigPayloadHint(len(payload)), "type": "upstream_error"},
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

// ---------- 网关流式工具消息 bug 自动降级（2026-09-26 定案，2026-10-19 解除） ----------
//
// 【2026-09-26 当时的根因】
// Qoder 网关的 DeepSeek 系（dfmodel/dmodel）与 Kimi 系（kmodel）在
// stream=true 且历史含 assistant(tool_calls) 或 role=tool 消息时，100% 返回
// provider_error（"Error in upstream response" / "Unknown sse issue"）；
// 同一载荷 stream=false 100% 成功；Qwen/GLM 系流式完全正常（多模型矩阵实测）。
// 这不是代理格式问题（转发的是标准 OpenAI 格式，网关自身即此语义）。
//
// 【2026-10-19 实测：上游已修复】
// 用户反馈「DeepSeek-Flash 半天不输出」。查日志发现 dfmodel 命中降级 19 次。
// 用探针直接向上游发 stream=true + 工具历史（dfmodel / 思考档 max）：
//   - 上游 HTTP 200，首字节 0.38s
//   - 解包后 5928 字节、23 个 data: 帧，unwrap 零报错
//   - tool_calls 帧完整：10 个增量帧，id/name/arguments/finish_reason=tool_calls 齐全
// 即：当年那个 provider_error 不再复现。
//
// 【为什么必须解除】
// 降级路径要把整个响应 collectStream 攒完再一次性回灌，实测同一请求：
//   真流式：首字节 0.207s，总耗时 1.138s（字一个个出来）
//   降级后：首字节 0.627s，总耗时 0.628s（憋完一次性吐出）
// 在 DSH/Codex 这类多轮工具调用场景里，这就是用户说的「半天不输出」。
//
// 【现在的策略】
// 默认走真流式。仍保留降级能力作为兜底，但改为「真流式失败后按需回退」：
// 由 QODER_AUTO_NOSTREAM 控制，默认不再预判式降级。
//   QODER_AUTO_NOSTREAM=1  → 恢复 2026-09-26 的预判式降级（老行为，应急用）
//   QODER_AUTO_NOSTREAM=0 或未设 → 真流式（当前默认）

// qoderStreamToolBugModels 曾受网关流式工具 bug 影响的模型内部 key。
// 保留此表仅供 QODER_AUTO_NOSTREAM=1 的应急回退路径使用。
var qoderStreamToolBugModels = map[string]bool{
	"dfmodel": true,
	"dmodel":  true,
	"kmodel":  true,
}

// qoderHistoryHasToolMsgs 判断历史里是否存在 assistant.tool_calls 或 role=tool 消息。
func qoderHistoryHasToolMsgs(msgs []any) bool {
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch mm["role"] {
		case "tool":
			return true
		case "assistant":
			if mm["tool_calls"] != nil {
				return true
			}
		}
	}
	return false
}

// qoderAutoNoStreamEnabled 是否启用「预判式降级」。
//
// 2026-10-19 起默认关闭：上游 bug 已不复现，而预判式降级会让
// DeepSeek/Kimi 系的流式请求变成「憋完一次性吐出」（用户报「半天不输出」）。
// 需要临时回到老行为时设 QODER_AUTO_NOSTREAM=1。
func qoderAutoNoStreamEnabled() bool {
	return os.Getenv("QODER_AUTO_NOSTREAM") == "1"
}

// degradedStreamToClient：上游 stream=false 聚合完整响应后，以 SSE 帧流回给客户端。
func (p *QoderProxy) degradedStreamToClient(w http.ResponseWriter, r *http.Request, payload []byte, model string, t0 time.Time, rid string) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	// 与 streamToClient 相同的保活策略：排队等待期先发 SSE 头 + 心跳
	flusher, _ := w.(http.Flusher)
	headersSent := false
	ensureHeaders := func() {
		if headersSent {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		headersSent = true
	}
	writeErrEvent := func(code int, msg string) {
		ensureHeaders()
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": code}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	onWait := func(waitSecs, totalSecs int) {
		ensureHeaders()
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{
			"index": 0, "finish_reason": nil, "delta": map[string]any{},
		}}}))
		if flusher != nil {
			flusher.Flush()
		}
	}
	resp, err := p.openInferValidated(ctx, payload, p.log, rid, onWait)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		var qe *qoderQueueError
		if errors.As(err, &qe) {
			writeErrEvent(qe.HTTPStatus(), qe.Error())
		} else {
			writeErrEvent(http.StatusBadGateway, err.Error())
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		if qi, ok := parseQueueFrame(string(b)); ok {
			writeErrEvent(http.StatusTooManyRequests, (&qoderQueueError{Info: qi}).Error())
			return
		}
		writeErrEvent(resp.StatusCode, truncate(string(b), 500))
		return
	}

	var inner bytes.Buffer
	if ferr := qoderUnwrapFrames(resp.Body, &inner); ferr != "" {
		if p.log != nil {
			p.log.Log("[%s] ✗ 上游错误帧（降级后仍失败）| %s", rid, truncate(ferr, 300))
		}
		msg, isErr := upstreamErrorFrame(ferr)
		if !isErr {
			msg = truncate(ferr, 500)
		}
		writeErrEvent(http.StatusBadGateway, msg+qoderBigPayloadHint(len(payload)))
		return
	}
	innerStr := qoderRewriteStreamForThinking(inner.String())
	col := collectStream(strings.NewReader(innerStr))

	ensureHeaders()

	// 聚合结果 → 标准 OpenAI 流式帧（客户端接口无感知）
	writeSSE := func(v any) {
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(v))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if rc := strings.Join(col.reasoning, ""); rc != "" {
		writeSSE(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": rc},
		}}})
	}
	if ct := strings.Join(col.contentParts, ""); ct != "" {
		writeSSE(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": ct},
		}}})
	}
	if len(col.toolCalls) > 0 {
		idx := make([]int, 0, len(col.toolCalls))
		for k := range col.toolCalls {
			idx = append(idx, k)
		}
		sort.Ints(idx)
		for _, k := range idx {
			v := col.toolCalls[k]
			id := v.id
			if id == "" {
				id = fmt.Sprintf("call_%d", k)
			}
			writeSSE(map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": k, "id": id, "type": "function",
						"function": map[string]any{"name": v.name, "arguments": v.arguments},
					}},
				},
			}}})
		}
	}
	finish := col.finish
	if finish == "" {
		finish = "stop"
	}
	writeSSE(map[string]any{"choices": []any{map[string]any{
		"index": 0, "delta": map[string]any{}, "finish_reason": finish,
	}}})
	writeSSE("[DONE]")

	elapsed := time.Since(t0)
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | degrade-stream | finish=%s | think=%d段", rid, elapsed.Seconds(), finish, len(col.reasoning))
	}
}
