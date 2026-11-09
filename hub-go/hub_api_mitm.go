package main

// MITM 分流代理的 HTTP API（控制台用）。
//
//	GET  /api/mitm          → 状态（是否运行、端口、CA 路径/指纹、分流计数、环境变量片段）
//	POST /api/mitm/start    → 启动代理
//	POST /api/mitm/stop     → 停止代理
//	GET  /api/mitm/ca.pem   → 下载 CA 证书（供 NODE_EXTRA_CA_CERTS 或手动导入）
//
// 刻意不提供「一键写入 ~/.claude/settings.json」这类自动化：环境变量要不要
// 进 shell profile、要不要信任一张能解密流量的 CA，由用户自己决定并自己动手。

import (
	"net/http"
	"os"
)

// apiMITM 处理状态查询与启停。
func (h *Hub) apiMITM(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mitm": h.mitmStatusSnapshot()})
	case http.MethodPost:
		action := r.URL.Query().Get("action")
		if action == "" {
			action = "start"
		}
		switch action {
		case "start":
			ok, msg := h.mitmStart()
			writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg, "mitm": h.mitmStatusSnapshot()})
		case "stop":
			ok, msg := h.mitmStop()
			writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg, "mitm": h.mitmStatusSnapshot()})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未知 action：" + action})
		}
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "只支持 GET / POST"})
	}
}

// apiMITMCA 下载 CA 证书。内容是公开证书，不含私钥。
func (h *Hub) apiMITMCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "只支持 GET"})
		return
	}
	b, err := os.ReadFile(mitmCACertPath())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok": false, "msg": "CA 证书不存在（先启动一次代理会自动生成）：" + err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="aihub-mitm-ca.pem"`)
	_, _ = w.Write(b)
}

// mitmEnsure 惰性初始化：第一次用到时才读/生成 CA（不在启动路径上做磁盘写）。
func (h *Hub) mitmEnsure() (*mitmProxy, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mitm != nil {
		return h.mitm, nil
	}
	ca, err := loadOrCreateMITMCA()
	if err != nil {
		return nil, err
	}
	h.mitm = newMITMProxy(ca)
	return h.mitm, nil
}

func (h *Hub) mitmStart() (bool, string) {
	p, err := h.mitmEnsure()
	if err != nil {
		return false, "本地 CA 初始化失败：" + err.Error()
	}
	return p.Start()
}

func (h *Hub) mitmStop() (bool, string) {
	h.mu.Lock()
	p := h.mitm
	h.mu.Unlock()
	if p == nil {
		return true, "未在运行"
	}
	return p.Stop()
}

// mitmStatusSnapshot 给控制台的完整快照（含环境变量片段）。
func (h *Hub) mitmStatusSnapshot() map[string]any {
	h.mu.Lock()
	p := h.mitm
	h.mu.Unlock()
	if p == nil {
		// 还没初始化过：仍然报告 CA 是否已在盘上，方便控制台决定按钮文案。
		_, err := os.Stat(mitmCACertPath())
		return map[string]any{
			"running":            false,
			"port":               mitmPort,
			"proxy_url":          "",
			"ca_path":            mitmCACertPath(),
			"ca_exists":          err == nil,
			"ca_fingerprint":     "",
			"requests":           0,
			"routed_local":       0,
			"routed_passthrough": 0,
			"tunneled":           0,
			"env_snippet":        mitmEnvSnippet(),
		}
	}
	st := p.status()
	return map[string]any{
		"running":            st.Running,
		"port":               st.Port,
		"proxy_url":          st.ProxyURL,
		"ca_path":            st.CAPath,
		"ca_exists":          st.CAExists,
		"ca_fingerprint":     st.CAFinger,
		"requests":           st.Requests,
		"routed_local":       st.Local,
		"routed_passthrough": st.Passthru,
		"tunneled":           st.Tunneled,
		"started_at":         st.StartedAt,
		"env_snippet":        mitmEnvSnippet(),
	}
}
