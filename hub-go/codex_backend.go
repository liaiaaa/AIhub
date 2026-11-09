package main

// Codex 原生 responses 路径（A′ 方案，2026-11-09）。
//
// 背景：config.toml 顶层只留 openai_base_url 指向
// http://127.0.0.1:8790/backend-api/codex 时，Codex 的 /responses 会落到这里
// （实测 responses_hits=13、models_hits=0，见规格第 1.1/1.2 节）。
// 本文件按 model 字段三分支：第三方 → aggDispatch；官方 slug → officialPassthrough；
// 都不命中 → 404 unknown_model（不猜测路由，不把未知模型悄悄发给官方）。

import (
	"bytes"
	"io"
	"net/http"
)

const codexUnknownModelMsg = "未知模型（既不在聚合白名单/线路前缀里，也不在本机 Codex 官方目录里）"

// codexBackendResponses 处理 POST /backend-api/codex/responses。
func (h *Hub) codexBackendResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "read body: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	slug := requestedModelSlug(body)
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体缺少 model 字段", "type": "invalid_request_error"},
		})
		return
	}
	// 1) 第三方：白名单 / ccs- / <线路>.<模型>
	if _, ok := resolveAggThirdParty(slug); ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.aggDispatch(w, r, "responses")
		return
	}
	// 2) 官方：按本机 Codex 官方 slug 集合判定（兼容 codex-auto-review 等非 gpt- 前缀）
	if codexOfficialSlugSet()[slug] {
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(body))
		h.officialPassthrough(w, r2, body)
		return
	}
	// 3) 都不命中
	recordUsage(usageRec{Line: "codex", Slug: slug, Kind: "responses", Code: http.StatusNotFound})
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]any{"message": codexUnknownModelMsg + "：" + slug, "type": "unknown_model"},
	})
}
