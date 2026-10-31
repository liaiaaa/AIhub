package main

// Hub 编排：代理启停、状态汇总、偏好持久化
// 由 main.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------- Hub ----------

type Hub struct {
	mu        sync.Mutex
	dom       *Proxy
	intl      *Proxy
	mimo      *Proxy
	qoder     *QoderProxy
	zcode     *ZcodeProxy
	trae      *TraeProxy
	oc        *OpencodeProxy // OpenCode 免费档（2026-10-15，只挂免费模型）
	intlCred  string
	domCred   string // 国内凭证（目录名）；空 = 传统解析（2026-10-13 国内多凭证）
	checkin   checkinRecord
	ctxWindow int // 向 Codex 宣告的上下文窗口上限（tokens），0=未设置用默认

	// lastLines 上次运行过的线路（dom/intl/qoder/trae/zcode/mimo/oc），
	// 随 state.json 持久化，进程启动时自动恢复（2026-10-19）。
	// 背景：2026-10-19 13:31 重启后只起了 agg+hub，dom/intl/qoder 全丢，
	// 其中 qoder 一直挂到被发现（聚合网关里两个 qoder 模型持续 503）。
	// 根因不是 switch 脚本探测有问题——是「线路在跑」这个事实只存在于内存里，
	// 进程一退就丢，重启时无从得知该恢复哪些。现在把它落到磁盘。
	lastLines []string
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
	if h.ctxWindow <= 0 {
		h.ctxWindow = 1000000
	}
	advertisedContextWindow.Store(int64(h.ctxWindow))
	return h
}

func (h *Hub) loadState() {
	b, err := os.ReadFile(dataPath("state.json"))
	if err != nil {
		return
	}
	var st struct {
		IntlCred  string         `json:"intl_cred"`
		DomCred   string         `json:"dom_cred"`
		Checkin   *checkinRecord `json:"checkin"`
		CtxWindow int            `json:"ctx_window"`
		LastLines []string       `json:"last_lines"`
	}
	if json.Unmarshal(b, &st) == nil {
		if st.IntlCred != "" {
			h.intlCred = st.IntlCred
		}
		h.domCred = st.DomCred
		if st.Checkin != nil {
			h.checkin = *st.Checkin
		}
		if st.CtxWindow > 0 {
			h.ctxWindow = st.CtxWindow
		}
		h.lastLines = sanitizeLineNames(st.LastLines)
	}
}

func (h *Hub) saveStateLocked() {
	b, err := json.Marshal(map[string]any{
		"intl_cred":  h.intlCred,
		"dom_cred":   h.domCred,
		"checkin":    h.checkin,
		"ctx_window": h.ctxWindow,
		"last_lines": h.lastLines,
	})
	if err != nil {
		logHub("state.json 序列化失败：%v", err)
		return
	}
	// tmp + rename（2026-10-31）：原来直接 os.WriteFile 且丢弃 err，
	// 写到一半掉电/被强杀就留半截 JSON，而 loadState 是静默忽略的 ——
	// 表现为「上次记住的线路、上下文窗口全没了」且日志里查不到任何原因。
	// 现在写失败会记日志，替换用 rename 保证 state.json 要么是旧的完整内容，
	// 要么是新的完整内容，不会出现半截。
	path := dataPath("state.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		logHub("state.json 写入临时文件失败：%v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logHub("state.json 替换失败：%v", err)
		_ = os.Remove(tmp)
	}
}

// ---------- 线路运行记忆（2026-10-19） ----------

// knownLineNames 可被记住/自动恢复的线路名（与 API 的 /api/<name>/start 对应）。
// 清单来自 lineSpecs（lines.go），不再手抄一份。
var knownLineNames = lineKeys()

// sanitizeLineNames 过滤未知线路名、去重、保持稳定顺序。
// 目的：state.json 是用户可手改的，不能因为里面写了个乱七八糟的名字
// 就去调一个不存在的 /api/<name>/start。
func sanitizeLineNames(in []string) []string {
	want := map[string]bool{}
	for _, n := range knownLineNames {
		want[n] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || !want[n] || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	// 按 knownLineNames 的固定顺序输出，避免每次写盘顺序抖动
	ordered := make([]string, 0, len(out))
	for _, n := range knownLineNames {
		if seen[n] {
			ordered = append(ordered, n)
		}
	}
	return ordered
}

// rememberLine 记下「这条线路现在在跑」，随 state.json 落盘。
// 调用点在各线路成功启动之后；停止时用 forgetLine 摘掉。
func (h *Hub) rememberLine(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := make([]string, 0, len(h.lastLines)+1)
	cur = append(cur, h.lastLines...)
	cur = append(cur, name)
	next := sanitizeLineNames(cur)
	if sameStrings(next, h.lastLines) {
		return
	}
	h.lastLines = next
	h.saveStateLocked()
}

// forgetLine 记下「这条线路已停」，重启后不再自动拉起它。
// 关键：用户主动停掉的线路不该在重启后自己回来。
func (h *Hub) forgetLine(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := make([]string, 0, len(h.lastLines))
	for _, n := range h.lastLines {
		if n != name {
			next = append(next, n)
		}
	}
	if sameStrings(next, h.lastLines) {
		return
	}
	h.lastLines = next
	h.saveStateLocked()
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// LinesToRestore 返回上次运行过、需要在本次启动时恢复的线路。
func (h *Hub) LinesToRestore() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.lastLines...)
}

// restoreLines 进程启动时把上次运行过的线路重新拉起来。
//
// 设计取舍：
//   - 异步执行（main 里 go 调用），任何一条线路启动慢/失败都不阻塞控制台。
//   - 逐条串行，失败不影响其余（某条凭据过期不该连累其它线路）。
//   - 成功后 rememberLine 会重复写同一个值，rememberLine 内部做了等值判断，
//     不会产生多余写盘。
//   - 恢复失败**不**从 last_lines 里摘掉：失败通常是凭据暂时不可用/上游故障，
//     下次重启还该再试一次。用户真想停掉就点控制台「停止」，那才会 forgetLine。
func (h *Hub) restoreLines() {
	lines := h.LinesToRestore()
	if len(lines) == 0 {
		return
	}
	logHub("自动恢复上次运行的线路：%v", lines)
	for _, name := range lines {
		ok, msg := h.startLineByName(name)
		if ok {
			logHub("  ✓ %s 已恢复：%s", name, msg)
			continue
		}
		logHub("  ✗ %s 恢复失败：%s", name, msg)
	}
}

// startLineByName 按线路名启动。返回 (是否成功, 说明)。
// 与 HTTP API 走同一批 startXxx，保证「自动恢复」和「手动点启动」行为一致。
//
// zcode 的 API Key 走 startZcode(apiKey)：自动恢复时传空串，
// 让它按既有优先级自己找（环境变量 / zcode_api_key.txt / 桌面端凭据解密）。
// 手工粘贴的 Key 只在当次进程内有效，本来就不该指望重启后还能用。
func (h *Hub) startLineByName(name string) (bool, string) {
	switch name {
	case "dom":
		return h.startDom()
	case "intl":
		h.mu.Lock()
		cred := h.intlCred
		h.mu.Unlock()
		return h.startIntl(cred)
	case "qoder":
		return h.startQoder()
	case "trae":
		return h.startTrae()
	case "zcode":
		return h.startZcode("")
	case "mimo":
		return h.startMimo()
	case "oc":
		return h.startOpencode()
	}
	return false, "未知线路：" + name
}

// contextWindow 返回当前向 Codex 宣告的上下文窗口上限（tokens）。
func (h *Hub) contextWindow() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctxWindow <= 0 {
		return 1000000
	}
	return h.ctxWindow
}

// ctxWindowLabel 把 token 数渲染成人类可读的窗口大小（1000000→"1M"、950000→"950K"）。
func ctxWindowLabel(n int) string {
	if n >= 1000000 && n%100000 == 0 {
		return strconv.Itoa(n/1000000) + "M"
	}
	if n >= 1000 && n%1000 == 0 {
		return strconv.Itoa(n/1000) + "K"
	}
	return strconv.Itoa(n)
}

// setContextWindow 更新宣告值：写内存 + 持久化 + 立即生效到 /v1/models 目录。
// 已启动的聚合口无需重启——codexModelsResponseFrom 每次请求都读当前值。
func (h *Hub) setContextWindow(n int) {
	h.mu.Lock()
	h.ctxWindow = n
	h.saveStateLocked()
	h.mu.Unlock()
	advertisedContextWindow.Store(int64(n))
}

// ---------- 代理启停 ----------
// 约定：慢操作（绑定端口、关闭监听）都在锁外做，锁内只做字段交换。
// 这样任何一个代理卡住都不会让整个控制台失去响应。

func (h *Hub) startDom() (bool, string) {
	h.mu.Lock()
	cred := h.domCred
	if h.dom != nil && h.dom.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	// 国内多凭证：选中的目录存在且有凭据文件时用目录，否则走传统解析。
	var p *Proxy
	if dir := domCredDir(cred); dir != "" && dirHasDomCredential(dir) {
		p = NewDomProxyDir(dir)
	} else {
		p = NewDomProxy()
	}
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
	h.rememberLine("dom")
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
	h.rememberLine("intl")
	return true, msg
}

// switchDom 换国内凭证并重启国内代理（apiDomSwitch 的实现，同 apiIntlSwitch 形状）。
func (h *Hub) switchDom(cred string) (bool, string) {
	if cred != "" {
		if _, ok := domCredDirs()[cred]; ok {
			h.mu.Lock()
			h.domCred = cred
			h.saveStateLocked()
			h.mu.Unlock()
		}
	}
	h.stopDom()
	h.mu.Lock()
	cred = h.domCred
	h.mu.Unlock()
	return h.startDomWithCred(cred)
}

// startDomWithCred 用指定凭证启动（不经 h.domCred，便于换凭证时明确目标）。
func (h *Hub) startDomWithCred(cred string) (bool, string) {
	h.mu.Lock()
	if h.dom != nil && h.dom.Running() {
		h.mu.Unlock()
		return true, "已在运行（换凭证请先停止）"
	}
	h.mu.Unlock()
	var p *Proxy
	if dir := domCredDir(cred); dir != "" && dirHasDomCredential(dir) {
		p = NewDomProxyDir(dir)
	} else {
		p = NewDomProxy()
	}
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.dom != nil && h.dom.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行（换凭证请先停止）"
	}
	h.dom = p
	h.mu.Unlock()
	// 2026-10-21 开源审查修的漏项：switchDom 先 stopDom()（里面会
	// forgetLine("dom")）再走这里，此前这条路径不补 rememberLine，导致
	// 「换过国内凭证」之后 state.json 的 last_lines 里 dom 已被摘掉，
	// 下次重启不再自动恢复。startDom 早就有这一行，startDomWithCred 漏了。
	h.rememberLine("dom")
	return true, msg
}

// stopDom / stopIntl 先摘掉引用再关监听，避免关闭过程中状态显示不一致。
func (h *Hub) stopDom() (bool, string) {
	h.mu.Lock()
	p := h.dom
	h.dom = nil
	h.mu.Unlock()
	if p == nil {
		h.forgetLine("dom")
		return true, "国内已停止"
	}
	if p.Stop() {
		h.forgetLine("dom")
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
		h.forgetLine("intl")
		return true, "国际已停止"
	}
	if p.Stop() {
		h.forgetLine("intl")
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
	h.rememberLine("mimo")
	return true, msg
}

func (h *Hub) stopMimo() (bool, string) {
	h.mu.Lock()
	p := h.mimo
	h.mimo = nil
	h.mu.Unlock()
	if p == nil {
		h.forgetLine("mimo")
		return true, "MiMo 已停止"
	}
	if p.Stop() {
		h.forgetLine("mimo")
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
	h.rememberLine("qoder")
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
	h.rememberLine("trae")
	return true, msg
}

func (h *Hub) stopTrae() (bool, string) {
	h.mu.Lock()
	p := h.trae
	h.trae = nil
	h.mu.Unlock()
	if p == nil {
		h.forgetLine("trae")
		return true, "Trae 已停止"
	}
	if p.Stop() {
		h.forgetLine("trae")
		return true, "Trae 已停止"
	}
	return false, "Trae 停止失败"
}

// startOpencode 起 OpenCode 免费档线路（2026-10-15）。
// 无需本机凭据：走的是上游公开的匿名 "public" 凭据。
func (h *Hub) startOpencode() (bool, string) {
	h.mu.Lock()
	if h.oc != nil && h.oc.Running() {
		h.mu.Unlock()
		return true, "已在运行"
	}
	h.mu.Unlock()

	p := NewOpencodeProxy()
	ok, msg := p.Start()
	if !ok {
		return false, msg
	}
	h.mu.Lock()
	if h.oc != nil && h.oc.Running() {
		h.mu.Unlock()
		p.Stop()
		return true, "已在运行"
	}
	h.oc = p
	h.mu.Unlock()
	h.rememberLine("oc")
	return true, msg
}

func (h *Hub) stopOpencode() (bool, string) {
	h.mu.Lock()
	p := h.oc
	h.oc = nil
	h.mu.Unlock()
	if p == nil {
		h.forgetLine("oc")
		return true, "OpenCode 已停止"
	}
	if p.Stop() {
		h.forgetLine("oc")
		return true, "OpenCode 已停止"
	}
	return false, "OpenCode 停止失败"
}

func (h *Hub) stopQoder() (bool, string) {
	h.mu.Lock()
	p := h.qoder
	h.qoder = nil
	h.mu.Unlock()
	if p == nil {
		h.forgetLine("qoder")
		return true, "Qoder 已停止"
	}
	if p.Stop() {
		h.forgetLine("qoder")
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
	h.rememberLine("zcode")
	return true, msg
}

func (h *Hub) stopZcode() (bool, string) {
	h.mu.Lock()
	p := h.zcode
	h.zcode = nil
	h.mu.Unlock()
	if p == nil {
		h.forgetLine("zcode")
		return true, "Zcode 已停止"
	}
	if p.Stop() {
		h.forgetLine("zcode")
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
	domCred := h.domCred
	traeP := h.trae
	ocP := h.oc
	h.mu.Unlock()

	dom := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if domP != nil && domP.Running() {
		dom["up"] = true
		dom["ok"] = true
		if domCred != "" {
			dom["detail"] = domCred // 显示当前国内凭证目录名（多凭证时更直观）
		} else {
			dom["detail"] = filepath.Base(domP.AuthFilePath())
		}
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
	// OpenCode 免费档（2026-10-15）：全部 0 计费模型，上下文由静态表标注。
	oc := map[string]any{"up": false, "ok": false, "nick": "", "detail": ""}
	if ocP != nil && ocP.Running() {
		oc["up"] = true
		oc["ok"] = true
		oc["detail"] = fmt.Sprintf("OpenCode 免费档 · %d 个免费模型 · 匿名 public 凭据", len(opencodeFreeModels))
		oc["models"] = len(opencodeFreeModels)
	}

	dirs := credDirs()
	creds := make([]string, 0, len(dirs))
	for k := range dirs {
		creds = append(creds, k)
	}
	sort.Strings(creds) // 固定顺序，避免下拉/单选框每次刷新都跳动
	return map[string]any{
		"dom":        dom,
		"intl":       intl,
		"mimo":       mimo,
		"qoder":      qoder,
		"zcode":      zcode,
		"trae":       trae,
		"oc":         oc,
		"intl_cred":  cred,
		"creds":      creds,
		"dom_cred":   domCred,
		"dom_creds":  domCredNames(),
		"default_cred": defaultIntlCred,
		"checkin":    h.checkinSnapshot(),
		"agg":        aggStatusEntry(),
		"ctx_window": h.contextWindow(),
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
