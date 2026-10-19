package main

// 官方 ChatGPT 透传线路（2026-10-13 共存方案）。
//
// 用户诉求：Codex 里官方模型和第三方模型共存，且保留官方登录身份。
// 机制：Codex 的 [model_providers.aihub] 带 requires_openai_auth = true，
// 请求会自动携带官方登录（auth.json）的 Authorization。统一网关把
// gpt-* 模型名**原样透传**到 chatgpt.com/backend-api/codex/responses
// （走系统代理），第三方模型照常走各线路。这样模型选择器一份列表：
//   - gpt-6-luna / gpt-5.6-terra / gpt-5.6-luna …→ 官方订阅计费
//   - intl.deepseek-v4.1-flash / dom.glm-5.3-flash …→ WorkBuddy 等积分计费
// auth.json 不读不改不落盘，登录身份完全保留。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const officialBackendURL = "https://chatgpt.com/backend-api/codex/responses"

// officialCatalogModels 模型选择器里展示的官方模型（请求侧不做白名单校验：
// gpt- 前缀一律透传，官方没这个名字时由官方 API 报错，避免网关侧失真）。
var officialCatalogModels = []string{"gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.6-sol"}

// isOfficialModelSlug gpt- 前缀即官方模型。放在 resolveAggModel 的最后一级，
// 不会与 白名单 / ccs- / <线路>.<模型> 形式冲突（没有以 gpt- 开头的线路键）。
func isOfficialModelSlug(slug string) bool {
	return strings.HasPrefix(slug, "gpt-")
}

// officialClient 透传客户端：chatgpt.com 国内直连不可达，复用 newBackendClient
// （读系统代理，与 intl 线路同一套网络路径）。流式响应不设总超时，
// 请求用 r.Context()，客户端断开即取消上游。
var officialClient = newBackendClient()

// officialHopHeaders 透传时丢弃的头（hop-by-hop 与代理无关字段）。
var officialSkipHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true,
	"accept-encoding": true, "keep-alive": true, "transfer-encoding": true,
}

// officialPassthrough 把 responses 请求原样转发给官方后端并流式回传。
func (h *Hub) officialPassthrough(w http.ResponseWriter, r *http.Request, nb []byte) {
	if r.Header.Get("Authorization") == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "官方模型需要 Codex 官方登录身份（config 里 aihub provider 须保留 requires_openai_auth = true，且 auth.json 已登录）。当前请求没带凭据。", "type": "auth_missing"},
		})
		return
	}
	nb = officialFixBody(nb)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, officialBackendURL, bytes.NewReader(nb))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "internal_error"},
		})
		return
	}
	for k, vs := range r.Header {
		if officialSkipHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := officialClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "官方后端不可达（检查系统代理是否可用）：" + err.Error(), "type": "upstream_error"},
		})
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "X-Request-Id", "Openai-Version", "Openai-Processing-Ms"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// officialFixBody 补官方后端的硬性要求：ChatGPT 账号走 codex 后端必须
// store=false（不落对话存储，2026-10-13 实测 "Store must be set to false"）。
// 客户端没带就补上；其余字段一律不动。
func officialFixBody(nb []byte) []byte {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(nb))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return nb
	}
	if _, ok := m["store"]; ok {
		return nb
	}
	m["store"] = false
	out, err := json.Marshal(m)
	if err != nil {
		return nb
	}
	return out
}

// officialCatalogModels 只保留给桌面版目录文件（codex_catalog.go）与测试引用。
// 2026-10-15：gpt-* 不再进 /v1/models 目录（用户要求目录只放 hub-go 线路模型，
// 不混官方模型），透传路由本身在 resolveAggModel 里保留。
// 原来的 officialCodexEntries / officialOpenAIEntries 已随目录改造删除。

// officialChatUnsupported chat 形态（OpenAI /chat/completions）用不了官方透传：
// auth.json 的 OAuth token 只对 codex 后端 /responses 有效，不是 api.openai.com 的
// API key。DSH 等客户端要官方模型只能走自家 API key，这里给明确报错。
const officialChatUnsupported = "官方模型（gpt-*）只支持 Codex（/v1/responses）形态：官方登录 token 不适用于 /chat/completions。DSH 请用各线路模型（intl./dom. 前缀）。"
