package main

// 线路显隐接口（2026-11-03）。
//
//	GET  /api/lines        → 全部线路 + hidden/running 标记（控制台渲染用）
//	POST /api/lines/hide   {"key":"mimo"} → 隐藏（若在跑先停掉）
//	POST /api/lines/show   {"key":"mimo"} → 恢复显示
//
// 隐藏不改配置、不删凭据，只是「不显示 + 不能启动 + 不进聚合」。

import (
	"encoding/json"
	"net/http"
)

// apiLines 返回全部线路元数据 + 运行态 + 显隐状态。
func (h *Hub) apiLines(w http.ResponseWriter, r *http.Request) {
	hidden := hiddenLines()
	lines := make([]map[string]any, 0, len(lineSpecs))
	for _, s := range lineSpecs {
		running := false
		if ad, ok := h.adapterFor(s.Key); ok && ad != nil {
			running = ad.Running()
		}
		lines = append(lines, map[string]any{
			"key":        s.Key,
			"display":    s.Display,
			"port":       s.Port,
			"log":        s.LogFile,
			"aggregated": s.Aggregated,
			"risk":       s.Risk,
			"hidden":     hidden[s.Key],
			"running":    running,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": lines})
}

// apiLineHide 隐藏一条线路：先停掉（如果它在跑），再写 lines.json。
func (h *Hub) apiLineHide(w http.ResponseWriter, r *http.Request) {
	key, ok := decodeLineKey(w, r)
	if !ok {
		return
	}
	stopped := ""
	if ad, ok := h.adapterFor(key); ok && ad != nil && ad.Running() {
		if _, msg := ad.Stop(); msg != "" {
			stopped = msg
		}
	}
	if err := setLineHidden(key, true); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logHub("线路已隐藏：%s%s", key, func() string {
		if stopped != "" {
			return "（" + stopped + "）"
		}
		return ""
	}())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已隐藏 " + lineDisplay(key)})
}

// apiLineShow 恢复显示。
func (h *Hub) apiLineShow(w http.ResponseWriter, r *http.Request) {
	key, ok := decodeLineKey(w, r)
	if !ok {
		return
	}
	if err := setLineHidden(key, false); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logHub("线路已恢复显示：%s", key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已显示 " + lineDisplay(key)})
}

// decodeLineKey 从请求体里取 key 并校验非空。
func decodeLineKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求体解析失败"})
		return "", false
	}
	if req.Key == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "缺少 key"})
		return "", false
	}
	return req.Key, true
}

// lineDisplay 线路显示名（找不到就返回 key 本身）。
func lineDisplay(key string) string {
	for _, s := range lineSpecs {
		if s.Key == key {
			return s.Display
		}
	}
	return key
}
