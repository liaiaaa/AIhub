package main

// 控制台 HTTP API：状态查询、启停、换凭证、退出
// 由 main.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)


func (h *Hub) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(webHTML))
}

func (h *Hub) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.statusMap())
}

// apiNick 单独返回账号昵称：可能涉及读文件/刷新，所以与 /api/status 分开，
// 保证状态轮询和启停操作永远不会被它拖住。
func (h *Hub) apiNick(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.enrichStatus())
}

func (h *Hub) apiDomStart(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.startDom()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiDomStop(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.stopDom()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiIntlStart(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	cred := h.intlCred
	h.mu.Unlock()
	ok, msg := h.startIntl(cred)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiIntlStop(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.stopIntl()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiMimoStart(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.startMimo()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiMimoStop(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.stopMimo()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiQoderStart(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.startQoder()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiQoderStop(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.stopQoder()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiIntlCred(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cred string `json:"cred"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Cred != "" {
		if _, ok := credDirs()[req.Cred]; ok {
			h.mu.Lock()
			h.intlCred = req.Cred
			h.saveStateLocked()
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "偏好已存：" + req.Cred})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未知凭证"})
}

func (h *Hub) apiIntlSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cred string `json:"cred"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Cred != "" {
		if _, ok := credDirs()[req.Cred]; ok {
			h.mu.Lock()
			h.intlCred = req.Cred
			h.saveStateLocked()
			h.mu.Unlock()
		}
	}
	h.stopIntl()
	h.mu.Lock()
	cred := h.intlCred
	h.mu.Unlock()
	ok, msg := h.startIntl(cred)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiStopAll(w http.ResponseWriter, r *http.Request) {
	a, _ := h.stopDom()
	b, _ := h.stopIntl()
	c, _ := h.stopMimo()
	d, _ := h.stopQoder()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  a && b && c && d,
		"msg": fmt.Sprintf("全部停止：国内=%s，国际=%s，MiMo=%s，Qoder=%s", okStr(a), okStr(b), okStr(c), okStr(d)),
	})
}

// apiExit 停掉两个代理后退出进程。
// 用带超时的 stop：即使某个代理有请求卡住，也保证进程一定退得掉。
func (h *Hub) apiExit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "正在退出…"})
	go func() {
		done := make(chan struct{})
		go func() {
			h.stopDom()
			h.stopIntl()
			h.stopMimo()
			h.stopQoder()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logHub("exit: 停止代理超时，强制退出")
		}
		logHub("hub exiting")
		os.Exit(0)
	}()
}

func okStr(b bool) string {
	if b {
		return "OK"
	}
	return "失败"
}
