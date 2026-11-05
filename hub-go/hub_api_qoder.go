package main

// Qoder 账号快照的 HTTP 接口（2026-11-05）。
//
//	GET  /api/qoder/accounts           → 当前凭据 + 全部快照
//	POST /api/qoder/accounts/save      {"name":"小号"}   → 把当前凭据存成快照
//	POST /api/qoder/accounts/use       {"name":"小号"}   → 切到某份快照
//	POST /api/qoder/accounts/delete    {"name":"小号"}   → 删快照
//
// 切换会影响「Qoder 代理」的登录态：调用方切完必须重启线路（或由前端提示），
// 因为 QoderProxy 在 Start 时把凭据读进内存（COSY 每请求签名，靠内存里的 token）。

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
)

func (h *Hub) apiQoderAccounts(w http.ResponseWriter, r *http.Request) {
	cur, curErr := qoderVaultCurrent()
	list, err := qoderVaultList()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "读取快照目录失败：" + err.Error()})
		return
	}
	resp := map[string]any{
		"ok":       true,
		"dir":      qoderVaultRoot(),
		"live":     filepath.Join(qoderAppDataDir(), "auth.v1.dat"),
		"running":  h.qoderRunning(),
		"accounts": list,
	}
	if curErr == nil {
		resp["current"] = cur
	} else {
		resp["current_error"] = curErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Hub) apiQoderAccountsSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求体解析失败"})
		return
	}
	if req.Name == "" {
		// 没给名字时用昵称，省得用户自己想
		if cur, err := qoderVaultCurrent(); err == nil && cur.Nick != "" {
			req.Name = cur.Nick
		}
	}
	meta, err := qoderVaultSave(req.Name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logHub("qoder 账号快照已保存：%s（%s / %s）", meta.Name, meta.Nick, meta.UID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "msg": "已保存快照 " + meta.Name, "account": meta,
	})
}

func (h *Hub) apiQoderAccountsUse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Start bool   `json:"start"` // 切换后是否顺手重启 Qoder 线路
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求体解析失败"})
		return
	}
	msg, err := qoderVaultUse(req.Name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	// 内存里的 QoderAuth 还是旧号的 token：重启线路才会重新解凭据。
	if h.qoderRunning() {
		h.stopQoder()
		if req.Start {
			if ok, m := h.startQoder(); !ok {
				msg += "；但重启线路失败：" + m
			} else {
				msg += "；线路已重启（" + m + "）"
			}
		} else {
			msg += "；线路已停止，需要时手动启动"
		}
	} else if req.Start {
		if ok, m := h.startQoder(); ok {
			msg += "；线路已启动（" + m + "）"
		} else {
			msg += "；启动失败：" + m
		}
	}
	logHub("qoder 账号切换：%s", msg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": msg})
}

func (h *Hub) apiQoderAccountsDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求体解析失败"})
		return
	}
	if err := qoderVaultDelete(req.Name); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logHub("qoder 账号快照已删除：%s", req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已删除快照 " + req.Name})
}

// apiQoderCreds Qoder 多凭证看板数据（快照列表 + 余额 + 状态）。
// 结构与 /api/dom/creds 完全一致，前端 renderCreds 不用改。
func (h *Hub) apiQoderCreds(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "creds": qoderCredsStatus(force)})
}

// apiQoderCred 点选凭证 = 把该快照写回 auth.v1.dat 并重启线路。
func (h *Hub) apiQoderCred(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cred string `json:"cred"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	ok, msg := h.qoderCredSwitch(req.Cred)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

// qoderRunning 当前 Qoder 线路是否在跑（nil 安全）。
func (h *Hub) qoderRunning() bool {
	h.mu.Lock()
	p := h.qoder
	h.mu.Unlock()
	return p != nil && p.Running()
}
