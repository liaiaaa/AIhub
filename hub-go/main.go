package main

// WorkBuddy Hub (Go 版) — 单进程：
//   - 控制台（网页，127.0.0.1:8801）：状态 / 启停 / 换凭证 / 退出
//   - 两个代理（国内 8787 / 国际 8799）在进程内按需启停
// 启动默认全部停止：先确认控制台端口可用，再清扫 8787/8799 残留监听。

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Windows API: 隐藏控制台窗口
var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
)

const SW_HIDE = 0 // 隐藏窗口

func hideConsoleWindow() {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, uintptr(SW_HIDE))
	}
}

const (
	DomPort  = 8787
	IntlPort = 8799
	HubPort  = 8801

	defaultIntlCred = "<凭证目录B>"
)

// hubRoot（数据根目录：凭证/日志/state.json 的所在处）见 paths.go。
// 它不再硬绑 exe 位置，解析规则与覆盖方式也一并写在那里。

func main() {
	port := flag.Int("port", HubPort, "控制台端口")
	noBrowser := flag.Bool("no-browser", false, "不自动打开浏览器")
	flag.Parse()

	// 先确认控制台端口能否绑定，再决定要不要清扫代理端口。
	// 顺序很重要：早期实现先 sweepPorts 再绑定，导致「重复双击 exe」时
	// 第二个实例会把第一个实例正在运行的代理端口 taskkill 掉（等于把在跑的服务打掉）。
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		// 已有实例在跑：只打开浏览器
		openBrowser(*port)
		return
	}

	// 默认全部停止：清扫 8787/8799 残留监听（连续约 6 秒，覆盖启动窗口期）
	sweepPorts()

	hub := NewHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/", hub.handleRoot)
	mux.HandleFunc("/api/status", hub.apiStatus)
	mux.HandleFunc("/api/nick", hub.apiNick)
	mux.HandleFunc("/api/dom/start", hub.apiDomStart)
	mux.HandleFunc("/api/dom/stop", hub.apiDomStop)
	mux.HandleFunc("/api/intl/start", hub.apiIntlStart)
	mux.HandleFunc("/api/intl/stop", hub.apiIntlStop)
	mux.HandleFunc("/api/mimo/start", hub.apiMimoStart)
	mux.HandleFunc("/api/mimo/stop", hub.apiMimoStop)
	mux.HandleFunc("/api/qoder/start", hub.apiQoderStart)
	mux.HandleFunc("/api/qoder/stop", hub.apiQoderStop)
	mux.HandleFunc("/api/intl/cred", hub.apiIntlCred)
	mux.HandleFunc("/api/intl/switch", hub.apiIntlSwitch)
	mux.HandleFunc("/api/stop_all", hub.apiStopAll)
	mux.HandleFunc("/api/exit", hub.apiExit)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logHub("console serve error: %v", err)
		}
	}()
	logHub("hub started on 127.0.0.1:%d", *port)
	if !*noBrowser {
		openBrowser(*port)
	}
	select {}
}

// ---------- Hub ----------

type Hub struct {
	mu       sync.Mutex
	dom      *Proxy
	intl     *Proxy
	mimo     *Proxy
	qoder    *QoderProxy
	intlCred string
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
		IntlCred string `json:"intl_cred"`
	}
	if json.Unmarshal(b, &st) == nil && st.IntlCred != "" {
		h.intlCred = st.IntlCred
	}
}

func (h *Hub) saveStateLocked() {
	b, _ := json.Marshal(map[string]any{"intl_cred": h.intlCred})
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

// ---------- 状态 ----------

// statusMap 只读内存中的启停状态，不做任何可能阻塞的调用（尤其不读凭据文件）。
// 早期实现在这里调 cred.Summary()，一旦 token 刷新卡住，状态轮询会占住全局锁，
// 导致「停止 / 退出」全部点不动——这正是「启动后关不掉」的根因。
func (h *Hub) statusMap() map[string]any {
	h.mu.Lock()
	domP, intlP, mimoP, qoderP, cred := h.dom, h.intl, h.mimo, h.qoder, h.intlCred
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
		"intl_cred":    cred,
		"creds":        creds,
		"default_cred": defaultIntlCred,
	}
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

// ---------- 端口工具 ----------

func portListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func pidsOnPort(port int) []int {
	var pids []int
	out, err := exec.Command("netstat", "-ano").Output()
	if err != nil {
		return pids
	}
	needle := fmt.Sprintf(":%d ", port)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "LISTENING") || !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	seen := map[int]bool{}
	uniq := pids[:0]
	for _, p := range pids {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

// killPid 只用于清理【启动前】残留在代理端口上的外部进程。
// 两道保险：
//  1. 绝不杀自己——代理跑在本进程内，端口上的 PID 往往就是 hub 自己，
//     早期版本在 Stop() 里调用它，等于「点停止 -> 自杀」。
//  2. 绝不杀同类 hub 进程——清扫的目标是残留（崩溃的旧进程 / Python 版转换器），
//     不是正在正常服务的另一个控制台实例。
func killPid(pid int) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	if isOwnProcessName(pid) {
		return
	}
	exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// isOwnProcessName 判断 pid 是否就是本程序（同名的另一个 hub 实例）。
func isOwnProcessName(pid int) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	want := strings.ToLower(filepath.Base(self))
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), `"`+want+`"`)
}

func stopPort(port int) bool {
	if portListening(port) {
		for _, pid := range pidsOnPort(port) {
			killPid(pid)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return !portListening(port)
}

// sweepPorts 启动时连续清扫，默认全部停止。
func sweepPorts() {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		a := stopPort(DomPort)
		b := stopPort(IntlPort)
		c := stopPort(MimoPort)
		d := stopPort(QoderPort)
		if a && b && c && d {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func openBrowser(port int) {
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func logHub(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	if f, err := os.OpenFile(dataPath("hub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		f.WriteString(line)
		f.Close()
	}
}
