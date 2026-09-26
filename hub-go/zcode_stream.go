package main

// Zcode 流改写与上游调用：原生 reasoning 透传、<think> 分流、聚合。
//
// Z.ai 的 SSE 是标准 OpenAI chunk 流（GLM 思考模型原生回 delta.reasoning_content），
// 无 Qoder 那种包装帧/provider_error 业务帧；但 <think> 标签分流复用同一套
// 状态机（qoder_stream.go 的分流器是通用的），保证任何形态的思考都能渲染。

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

// zcodeUpstream 调用 Z.ai chat/completions，带自愈重试：
//   - 网络错误 / 429 / 5xx：等 700ms 重试（最多 3 次尝试）
//   - 401：重读桌面端凭据（ZCode 桌面端重新登录后 credentials.json 会更新）
//   - 重试耗尽：把最后一个真实响应交上层转发（携带上游状态码与错误体）
//
// 返回的 resp 由调用方关闭。
func (p *ZcodeProxy) zcodeUpstream(ctx context.Context, payload []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(700 * time.Millisecond)
		}
		req, rerr := http.NewRequestWithContext(ctx, "POST", p.Endpoint+"/chat/completions",
			bytes.NewReader(payload))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
		req.Header.Set("Accept-Language", "en-US,en")
		resp, err = p.client.Do(req)
		if err != nil {
			if attempt == 2 {
				return nil, err
			}
			continue // 网络抖动重试
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode >= 500 {
			status := resp.StatusCode
			if attempt == 2 {
				return resp, nil // 重试耗尽：原样交上层转发真实错误
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			resp = nil
			if status == http.StatusUnauthorized && p.auth != nil {
				if rerr := p.auth.Reload(); rerr == nil {
					p.apiKey = p.auth.Key()
				}
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

// zcodeHintError 把上游业务错误转成对用户可操作的提示。
// 1113 = 订阅计划额度只能由桌面端 App 自己的中继消费（该中继要求阿里云验证码 +
// 设备身份，第三方无法直连），API Key 直连路径只对充值余额/资源包生效。
func zcodeHintError(msg string) string {
	if strings.Contains(msg, "1113") || strings.Contains(msg, "Insufficient balance") {
		return "该 Zcode 订阅计划（v3 Start Plan）的额度只能由 Zcode 桌面端自身消费：" +
			"网关中继要求桌面端内置的验证码/设备校验，API Key 直连会报 1113。" +
			"若要经代理使用，请在 Z.ai 充值余额或购买资源包（走通用 API 端点），或改用桌面端内的 Zcode 会话。原始错误：" + msg
	}
	return msg
}

func (p *ZcodeProxy) streamToClient(w http.ResponseWriter, r *http.Request, payload []byte, t0 time.Time, rid string) {
	thinkFrames := 0
	resp, err := p.zcodeUpstream(r.Context(), payload)
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

	// 逐帧转发；<think>/<thinking> 标签内容分流到 reasoning_content，
	// 原生 reasoning_content 帧不改动直接透传（qoderRewriteChunk 无标签时原样返回）。
	st := newQoderThinkState()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	upstreamErrs := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") || !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[5:])
		if raw == "[DONE]" {
			// 残留缓冲必须在 [DONE] 之前吐出（DONE 之后的帧会被客户端忽略）
			if out, think := st.flush(); out != "" || think != "" {
				qoderWriteTailFrame(w, flusher, out, think)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		// 上游错误帧（{"error":{...}}，OpenAI 标准形态）：原样转发保证可见
		var probe map[string]any
		if json.Unmarshal([]byte(raw), &probe) == nil {
			if eobj, hasErr := probe["error"].(map[string]any); hasErr {
				if _, hasChoices := probe["choices"]; !hasChoices {
					upstreamErrs++
					m, _ := eobj["message"].(string)
					if p.log != nil {
						p.log.Log("[%s] ✗ 上游错误帧 #%d | %s", rid, upstreamErrs, truncate(raw, 300))
					}
					if m != "" {
						eobj["message"] = zcodeHintError(m)
					}
					fmt.Fprintf(w, "data: %s\n\n", mustJSON(probe))
					if flusher != nil {
						flusher.Flush()
					}
					continue
				}
			}
		}
		rw := qoderRewriteChunk(raw, st, false)
		if strings.Contains(rw, `"reasoning_content"`) {
			thinkFrames++
		}
		fmt.Fprintf(w, "data: %s\n\n", rw)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 上游没发 [DONE] 就断流时兜底吐残留（flush 后缓冲为空，重复调用无副作用）
	if out, think := st.flush(); out != "" || think != "" {
		qoderWriteTailFrame(w, flusher, out, think)
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | stream | think=%d帧", rid, time.Since(t0).Seconds(), thinkFrames)
	}
}

func (p *ZcodeProxy) aggregateToClient(w http.ResponseWriter, r *http.Request, payload []byte, model string, t0 time.Time, rid string) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	resp, err := p.zcodeUpstream(ctx, payload)
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

	// Z.ai 有原生非流式形态：直接透传（补 <think> 分流处理的只有流式聚合路径）。
	// 流式聚合路径：<think> 转写 → collectStream（tool_calls/usage/reasoning 全支持）。
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	trimmed := strings.TrimSpace(string(bodyBytes))
	if strings.HasPrefix(trimmed, "data:") {
		innerStr := qoderRewriteStreamForThinking(trimmed)
		col := collectStream(strings.NewReader(innerStr))
		if col.model != "" && col.model != "unknown" {
			model = col.model
		}
		tokens := "?"
		if col.usage != nil {
			if v, ok := col.usage["total_tokens"].(json.Number); ok {
				tokens = v.String()
			}
		}
		if p.log != nil {
			p.log.Log("[%s] ◀ RESPONSE | %.1fs | finish=%s | tokens=%s | think=%d段",
				rid, time.Since(t0).Seconds(), col.finish, tokens, len(col.reasoning))
		}
		res := col.build()
		res["model"] = model
		writeJSON(w, http.StatusOK, res)
		return
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | 非流式直传", rid, time.Since(t0).Seconds())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bodyBytes)
}
