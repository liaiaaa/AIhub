package main

// 控制台 HTTP API：状态查询、启停、换凭证、退出
// 由 main.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// apiGateway 返回 Codex 控制面（Hub Switch / 原版 cc-switch 的本地路由 :15721）
// 的聚合状态，供仪表盘「网关健康提示」与「聚合与网关」页展示。
//
// 分工原则（2026-10-13 用户拍板）：聚合的供应商/模型管理以 Hub Switch 为准，
// hub-go 只读展示、不做重复的管理界面。数据来源：
//   - :15721 TCP 探测（通不通）；
//   - ~/.cc-switch/live-state.json 的 apps.codex（mode/attached/stack 成员）；
//   - ~/.codex/cc-switch-model-catalog.json（聚合发布的模型目录，best-effort）。
// 后两者不在 hub-go 管辖内，读不到就只报端口状态，不报错。
// apiGateway 统一网关状态。
//
// 2026-10-15 改造：原实现报告的是 Hub Switch/cc-switch 时代的本地路由（:15721）状态，
// 方案 C 后 hub-go 自己就是网关（:8790），那些字段会让人误判"网关没起来"
// （实测 :8790 已 up，该端点却返回 up=false、mode=direct）。
// 现在主字段改报 :8790 的真实状态；:15721 相关字段保留并改名加 _legacy 后缀，
// 避免前端读到 undefined，也表明那是历史字段。
func (h *Hub) apiGateway(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"ok": true,
	}
	// 主字段：hub-go 自己的统一网关 :8790
	aggSrvMu.Lock()
	up := aggSrv != nil
	aggSrvMu.Unlock()
	resp["port"] = aggPort
	resp["up"] = up
	if up {
		resp["mode"] = "gateway"
	} else {
		resp["mode"] = "stopped"
	}
	// 网关模型目录（聚合白名单 + 倍率/上下文，与 /v1/models 同源）
	list := aggSnapshot()
	models := make([]string, 0, len(list))
	names := make([]string, 0, len(list))
	for _, am := range list {
		models = append(models, am.Slug)
		names = append(names, aggOpenAIDisplayName(h, am))
	}
	resp["models"] = models
	resp["model_names"] = names

	// 遗留字段（Hub Switch 时代，仅供前端兼容读取，勿用于判断网关是否可用）
	const legacyRouterPort = 15721
	legacyUp := false
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(legacyRouterPort), 1500*time.Millisecond); err == nil {
		conn.Close()
		legacyUp = true
	}
	resp["legacy_router_port"] = legacyRouterPort
	resp["legacy_router_up"] = legacyUp
	if home, err := os.UserHomeDir(); err == nil {
		if raw, err := os.ReadFile(filepath.Join(home, ".cc-switch", "live-state.json")); err == nil {
			var st struct {
				Apps struct {
					Codex struct {
						Mode       string `json:"mode"`
						Attached   bool   `json:"attached"`
						ProxyRoute string `json:"proxy_route"`
						Stack      struct {
							Enabled bool     `json:"enabled"`
							Members []string `json:"members"`
						} `json:"stack"`
					} `json:"codex"`
				} `json:"apps"`
			}
			if json.Unmarshal(raw, &st) == nil {
				resp["legacy_mode"] = st.Apps.Codex.Mode
				resp["legacy_attached"] = st.Apps.Codex.Attached
				resp["legacy_default_provider"] = st.Apps.Codex.ProxyRoute
				resp["legacy_stack_enabled"] = st.Apps.Codex.Stack.Enabled
				resp["legacy_stack_members"] = st.Apps.Codex.Stack.Members
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

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

// lineToggle 一条线路的启停实现（A2：把 12 个一模一样的 handler 收敛成一张表）。
//
// 此前每条线路都写了两个 handler（apiDomStart/apiDomStop、apiQoderStart/…），
// 每个 4 行里有 3 行是同一句 writeJSON。加一条线路要改 4 处：路由 2 行 + handler 2 个。
// 现在只需要在 lines.go 加一行元数据、在这张表加一行启停实现。
//
// 两个特例不在表里：
//   - zcode 的 start 需要 apiKey（见 apiZcodeStart）；
//   - intl 的 start 需要当前选中的国际凭证，用闭包适配，读法与原 handler 完全一致。
type lineToggle struct {
	Start func(h *Hub) (bool, string)
	Stop  func(h *Hub) (bool, string)
}

var lineToggles = map[string]lineToggle{
	"dom": {
		Start: func(h *Hub) (bool, string) { return h.startDom() },
		Stop:  func(h *Hub) (bool, string) { return h.stopDom() },
	},
	"intl": {
		Start: func(h *Hub) (bool, string) {
			h.mu.Lock()
			cred := h.intlCred
			h.mu.Unlock()
			return h.startIntl(cred)
		},
		Stop: func(h *Hub) (bool, string) { return h.stopIntl() },
	},
	"mimo": {
		Start: func(h *Hub) (bool, string) { return h.startMimo() },
		Stop:  func(h *Hub) (bool, string) { return h.stopMimo() },
	},
	"qoder": {
		Start: func(h *Hub) (bool, string) { return h.startQoder() },
		Stop:  func(h *Hub) (bool, string) { return h.stopQoder() },
	},
	"trae": {
		Start: func(h *Hub) (bool, string) { return h.startTrae() },
		Stop:  func(h *Hub) (bool, string) { return h.stopTrae() },
	},
	// OpenCode 免费档线路（2026-10-15）：不需要本机凭据，起即可用
	// （上游公开匿名 "public" 凭据，只放免费模型）。
	"oc": {
		Start: func(h *Hub) (bool, string) { return h.startOpencode() },
		Stop:  func(h *Hub) (bool, string) { return h.stopOpencode() },
	},
}

// apiLineStart 返回某条线路的启动 handler。响应体与收敛前的专用 handler 完全一致。
func (h *Hub) apiLineStart(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := lineToggles[key]
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未知线路：" + key})
			return
		}
		started, msg := t.Start(h)
		writeJSON(w, http.StatusOK, map[string]any{"ok": started, "msg": msg})
	}
}

// apiLineStop 返回某条线路的停止 handler。
func (h *Hub) apiLineStop(key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := lineToggles[key]
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未知线路：" + key})
			return
		}
		stopped, msg := t.Stop(h)
		writeJSON(w, http.StatusOK, map[string]any{"ok": stopped, "msg": msg})
	}
}

// apiDomCred 记住国内凭证偏好（不重启）。
func (h *Hub) apiDomCred(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cred string `json:"cred"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Cred != "" {
		if _, ok := domCredDirs()[req.Cred]; ok {
			h.mu.Lock()
			h.domCred = req.Cred
			h.saveStateLocked()
			h.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "偏好已存：" + req.Cred})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "未知凭证"})
}

// apiDomSwitch 换国内凭证并重启国内代理（对齐 apiIntlSwitch）。
func (h *Hub) apiDomSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cred string `json:"cred"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	ok, msg := h.switchDom(req.Cred)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

// apiModelsProbe 一键探测各服务商是否上新模型（免费模型优先）。
// 见 model_probe.go。
func (h *Hub) apiModelsProbe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lines": probeAllModels(h)})
}

// apiZcodeStart 需要 apiKey，因此不走 lineToggles 表。
func (h *Hub) apiZcodeStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求格式错误"})
		return
	}

	ok, msg := h.startZcode(req.APIKey)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiZcodeStop(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.stopZcode()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiZcodeKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求格式错误"})
		return
	}

	if req.APIKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "API Key 不能为空"})
		return
	}

	if err := SaveAPIKey(req.APIKey); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "保存失败：" + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "API Key 已保存"})
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

// apiCheckin 一键签到：国内 WorkBuddy + Qoder 并发执行，结果落 state.json。
func (h *Hub) apiCheckin(w http.ResponseWriter, r *http.Request) {
	ok, msg := runCheckin(h)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

// apiIntlCreds 汇总全部国际凭证的可用性（哪个能用 / 限流到几点 / 余额多少）。
// 这是「限流看板」的数据源：让用户不必记「被限到几点」。
func (h *Hub) apiIntlCreds(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "creds": intlCredsStatus(h, force)})
}

// apiDomCreds 汇总全部国内凭证的可用性（卡片看板，与国际版同一接口形状）。
//
// 用户 2026-10-17 要求把国内的凭证切换从 <select> 改成国际版那样的卡片：
// 光换皮不够，得先有「每个凭证能不能用、限到几点、余额多少」这份数据，
// 否则卡片上只能显示名字，比下拉框还没信息量。
func (h *Hub) apiDomCreds(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "creds": domCredsStatus(h, force)})
}

// apiCtxWindow 读写「向 Codex 宣告的上下文窗口上限」。
// GET 返回当前值；POST {"tokens":N} 设置（写内存 + state.json，立即生效）。
// 作用：让 Codex 更早触发自动压缩，避免历史逼近上游 1M 硬顶时撞 400（code 11115）。
func (h *Hub) apiCtxWindow(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Tokens int `json:"tokens"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "bad json: " + err.Error()})
			return
		}
		if req.Tokens < 8000 || req.Tokens > 2000000 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "上下文窗口需在 8K ~ 2M 之间"})
			return
		}
		h.setContextWindow(req.Tokens)
		codexCatalogRefresh() // 桌面版目录文件同步宣告值（2026-10-13 修复 950k 不同步）
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已设为 " + ctxWindowLabel(req.Tokens) + "（Codex 端重开会话或刷新模型目录后生效）", "tokens": req.Tokens})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tokens": h.contextWindow()})
}

// apiIntlCheckin 单独给全部国际凭证做活跃上报（不碰国内/Trae）。
func (h *Hub) apiIntlCheckin(w http.ResponseWriter, r *http.Request) {
	ok, msg := runIntlCheckin(h)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

// apiCredits 各平台剩余额度（5 分钟缓存，?force=1 作废缓存后台补拉）。
// 带 ?key=<平台> 时只返回该平台（瓦片独立刷新），形如 {"intl":{...}}。
func (h *Hub) apiCredits(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	key := r.URL.Query().Get("key")
	if key != "" {
		info, ok := creditsOne(h, key, force)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"message": "未知平台：" + key, "type": "invalid_request_error"},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]creditInfo{key: info})
		return
	}
	writeJSON(w, http.StatusOK, creditsSnapshot(h, force))
}

func (h *Hub) apiStopAll(w http.ResponseWriter, r *http.Request) {
	a, _ := h.stopDom()
	b, _ := h.stopIntl()
	c, _ := h.stopMimo()
	d, _ := h.stopQoder()
	e, _ := h.stopZcode()
	f, _ := h.stopTrae()
	// OpenCode 免费档也得跟着停：漏了会让控制台点「全部停止」后 :8800 仍留监听。
	g, _ := h.stopOpencode()
	i, _ := h.stopAggLine()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  a && b && c && d && e && f && g && i,
		"msg": fmt.Sprintf("全部停止：国内=%s，国际=%s，MiMo=%s，Qoder=%s，Zcode=%s，Trae=%s，OpenCode=%s，聚合=%s", okStr(a), okStr(b), okStr(c), okStr(d), okStr(e), okStr(f), okStr(g), okStr(i)),
	})
}

// apiExit 停掉所有代理后退出进程。
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
			h.stopZcode()
			h.stopTrae()
			h.stopOpencode()
			h.stopAggLine()
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
