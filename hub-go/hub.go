package main

// Hub 编排：代理启停、状态汇总、偏好持久化
// 由 main.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ---------- Hub ----------

type Hub struct {
	mu       sync.Mutex
	dom      *Proxy
	intl     *Proxy
	mimo     *Proxy
	qoder    *QoderProxy
	zcode    *ZcodeProxy
	trae     *TraeProxy
	intlCred string
	checkin  checkinRecord
}

// checkinRecord 最近一次签到结果（逻辑见 checkin.go），随 state.json 持久化，
// 控制台据此展示「今日已签/未签」。
type checkinRecord struct {
	Date  string `json:"date"`
	Time  string `json:"time"`
	Dom   string `json:"dom"`
	Intl  string `json:"intl"`
	Qoder string `json:"qoder"`
	Trae  string `json:"trae"`
	OK    bool   `json:"ok"`
}

func NewHub() *Hub {
	h := &Hub{intlCred: defaultIntlCred}
	h.loadState()
	return h
}

func (h *Hub) loadState() {
	b, err := os.ReadFile(dataPath("state.json"))
	if err != nil {
		return
	}
	var st struct {
		IntlCred string         `json:"intl_cred"`
		Checkin  *checkinRecord `json:"checkin"`
	}
	if json.Unmarshal(b, &st) == nil {
		if st.IntlCred != "" {
			h.intlCred = st.IntlCred
		}
		if st.Checkin != nil {
			h.checkin = *st.Checkin
		}
	}
}

func (h *Hub) saveStateLocked() {
	b, _ := json.Marshal(map[string]any{"intl_cred": h.intlCred, "checkin": h.checkin})
	os.WriteFile(dataPath("state.json"), b, 0644)
}

// ---------- 代理启停 ----------
// 约定：慢操作（绑定端口、关闭监听）都在锁外做，锁内只做字段交换。
// 这样任何一个代理卡住都不会让整个控制台失去响应。

func (h *Hub) startDom() (bool, string) {
	h.mu.Lock()
	if h.dom != nil && h.dom.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	p := NewDomProxy()
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	// 并发点击时可能已有人先启动成功，保留先到的那个。
	if h.dom != nil && h.dom.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行"
	}
	h.dom = p
	h.mu.Unlock()
	return true, msg
}

func (h *Hub) startIntl(cred string) (bool, string) {
	h.mu.Lock()
	if h.intl != nil && h.intl.Running() {
		h.mu.Unlock()
		return true, "已在运行（换凭证请先停止）"
	}
	h.mu.Unlock()

	dir := credDirs()[cred]
	if dir == "" {
		return false, "凭证目录不存在: " + cred
	}
	if !dirHasCredential(dir) {
		return false, "目录里没有 .info 凭证文件：" + dir
	}
	p := NewIntlProxy(dir)
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.intl != nil && h.intl.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行（换凭证请先停止）"
	}
	h.intl = p
	h.mu.Unlock()
	return true, msg
}

// stopDom / stopIntl 先摘掉引用再关监听，避免关闭过程中状态显示不一致。
func (h *Hub) stopDom() (bool, string) {
	h.mu.Lock()
	p := h.dom
	h.dom = nil
	h.mu.Unlock()
	if p == nil {
		return true, "国内已停止"
	}
	if p.Stop() {
		return true, "国内已停止"
	}
	return false, "国内停止失败"
}

func (h *Hub) stopIntl() (bool, string) {
	h.mu.Lock()
	p := h.intl
	h.intl = nil
	h.mu.Unlock()
	if p == nil {
		return true, "国际已停止"
	}
	if p.Stop() {
		return true, "国际已停止"
	}
	return false, "国际停止失败"
}

// startMimo 启动 MiMo 代理。认证走小米 SSO，凭据来自桌面端登录 cookie。
func (h *Hub) startMimo() (bool, string) {
	h.mu.Lock()
	if h.mimo != nil && h.mimo.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	p := NewMimoProxy()
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.mimo != nil && h.mimo.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行"
	}
	h.mimo = p
	h.mu.Unlock()
	return true, msg
}

func (h *Hub) stopMimo() (bool, string) {
	h.mu.Lock()
	p := h.mimo
	h.mimo = nil
	h.mu.Unlock()
	if p == nil {
		return true, "MiMo 已停止"
	}
	if p.Stop() {
		return true, "MiMo 已停止"
	}
	return false, "MiMo 停止失败"
}

// startQoder 启动 Qoder CN 代理。凭据零配置：启动时 DPAPI 解密桌面端登录态。
// 注意：8797 上若残留独立版 qoder_proxy.exe，Start 内部会先清扫再绑定。
func (h *Hub) startQoder() (bool, string) {
	h.mu.Lock()
	if h.qoder != nil && h.qoder.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	p := NewQoderProxy()
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.qoder != nil && h.qoder.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行"
	}
	h.qoder = p
	h.mu.Unlock()
	return true, msg
}

func (h *Hub) startTrae() (bool, string) {
	h.mu.Lock()
	if h.trae != nil && h.trae.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	p := NewTraeProxy()
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.trae != nil && h.trae.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行"
	}
	h.trae = p
	h.mu.Unlock()
	return true, msg
}

func (h *Hub) stopTrae() (bool, string) {
	h.mu.Lock()
	p := h.trae
	h.trae = nil
	h.mu.Unlock()
	if p == nil {
		return true, "Trae 已停止"
	}
	if p.Stop() {
		return true, "Trae 已停止"
	}
	return false, "Trae 停止失败"
}

func (h *Hub) stopQoder() (bool, string) {
	h.mu.Lock()
	p := h.qoder
	h.qoder = nil
	h.mu.Unlock()
	if p == nil {
		return true, "Qoder 已停止"
	}
	if p.Stop() {
		return true, "Qoder 已停止"
	}
	return false, "Qoder 停止失败"
}

// startZcode 启动 Zcode 代理。需要设置 API Key。
// API Key 可以通过环境变量 ZCODE_API_KEY 或数据目录的 zcode_api_key.txt 文件设置。
func (h *Hub) startZcode(apiKey string) (bool, string) {
	h.mu.Lock()
	if h.zcode != nil && h.zcode.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	p := NewZcodeProxy()
	if apiKey != "" {
		p.APIKey = apiKey
	}
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.zcode != nil && h.zcode.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行"
	}
	h.zcode = p
	h.mu.Unlock()
	return true, msg
}

func (h *Hub) stopZcode() (bool, string) {
	h.mu.Lock()
	p := h.zcode
	h.zcode = nil
	h.mu.Unlock()
	if p == nil {
		return true, "Zcode 已停止"
	}
	if p.Stop() {
		return true, "Zcode 已停止"
	}
	return false, "Zcode 停止失败"
}

// ---------- 状态 ----------

// statusMap 只读内存中的启停状态，不做任何可能阻塞的调用（尤其不读凭据文件）。
// 早期实现在这里调 cred.Summary()，一旦 token 刷新卡住，状态轮询会占住全局锁，
// 导致「停止 / 退出」全部点不动——这正是「启动后关不掉」的根因。
func (h *Hub) statusMap() map[string]any {
	h.mu.Lock()
	domP, intlP, mimoP, qoderP, zcodeP, cred := h.dom, h.intl, h.mimo, h.qoder, h.zcode, h.intlCred
	traeP := h.trae
	h.mu.Unlock()

	dom := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if domP != nil && domP.Running() {
		dom["up"] = true
		dom["ok"] = true
		dom["detail"] = filepath.Base(domP.AuthFilePath())
	}
	intl := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if intlP != nil && intlP.Running() {
		intl["up"] = true
		intl["ok"] = true
		if intlP.AuthDir != "" {
			intl["detail"] = filepath.Base(intlP.AuthDir)
		}
	}
	mimo := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if mimoP != nil && mimoP.Running() {
		mimo["up"] = true
		mimo["ok"] = true
		mimo["detail"] = "小米账号 SSO"
	}
	qoder := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if qoderP != nil && qoderP.Running() {
		qoder["up"] = true
		qoder["ok"] = true
		catalog := qoderP.catalogSnapshot()
		qoder["detail"] = fmt.Sprintf("COSY 直连 · %d 个模型 · 桌面端凭据自动解密", len(catalog))
		qoder["models"] = len(catalog)
	}
	zcode := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if zcodeP != nil && zcodeP.Running() {
		zcode["up"] = true
		zcode["ok"] = true
		zcode["detail"] = zcodeStatusDetail(zcodeP)
		zcode["models"] = len(zcodeP.catalogSnapshot())
	}
	trae := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if traeP != nil && traeP.Running() {
		trae["up"] = true
		trae["ok"] = true
		trae["detail"] = fmt.Sprintf("SOLO 免费通道 · %d 个模型 · 桌面端凭据自动解密", len(TraeModels))
		trae["models"] = len(TraeModels)
	}

	dirs := credDirs()
	creds := make([]string, 0, len(dirs))
	for k := range dirs {
		creds = append(creds, k)
	}
	sort.Strings(creds) // 固定顺序，避免下拉/单选框每次刷新都跳动
	return map[string]any{
		"dom":          dom,
		"intl":         intl,
		"mimo":         mimo,
		"qoder":        qoder,
		"zcode":        zcode,
		"trae":         trae,
		"intl_cred":    cred,
		"creds":        creds,
		"default_cred": defaultIntlCred,
		"checkin":      h.checkinSnapshot(),
		"agg":          aggStatusEntry(),
	}
}

// checkinSnapshot 最近签到结果的只读副本（checkin.go 写入）。
func (h *Hub) checkinSnapshot() checkinRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checkin
}

// enrichStatus 异步补充账号昵称，供前端下一次轮询取用。
// 单独走一个轻量接口语义：即使凭据读取慢，也绝不阻塞启停/退出。
func (h *Hub) enrichStatus() map[string]any {
	out := map[string]any{}
	h.mu.Lock()
	domP, intlP, mimoP, qoderP := h.dom, h.intl, h.mimo, h.qoder
	h.mu.Unlock()
	for key, p := range map[string]*Proxy{"dom": domP, "intl": intlP, "mimo": mimoP} {
		if p == nil || !p.Running() {
			continue
		}
		auth, _, _ := p.runtime()
		if auth == nil {
			continue
		}
		if s, err := auth.Summary(); err == nil {
			if n, ok := s["nickname"].(string); ok && n != "" {
				out[key] = n
			}
		}
	}
	// Qoder 的认证不走 AuthProvider（COSY 每请求签名），昵称直接取内存快照。
	if qoderP != nil && qoderP.Running() && qoderP.auth != nil {
		if name, _, _, _ := qoderP.auth.Snapshot(); name != "" {
			out["qoder"] = name
		}
	}
	return out
}

// ---------- HTTP API ----------
