package main

// 工具函数：JSON 响应、类型归一、系统代理探测
// 由 proxy.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 工具函数 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func sseError(w http.ResponseWriter, code int, msg string) {
	chunk := map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": code}}
	b, _ := json.Marshal(chunk)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func cloneMap(m map[string]any) map[string]any {
	nm := make(map[string]any, len(m))
	for k, v := range m {
		nm[k] = v
	}
	return nm
}

func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func intVal(v any, def int) int {
	switch n := v.(type) {
	case json.Number:
		if i, err := strconv.Atoi(n.String()); err == nil {
			return i
		}
	case float64:
		return int(n)
	}
	return def
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func lastUserSuffix(msgs []any) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			return " | last_user=" + strconv.Quote(truncate(c, 60))
		case []any:
			for _, blk := range c {
				if bm, ok := blk.(map[string]any); ok && bm["type"] == "text" {
					if t, ok := bm["text"].(string); ok {
						return " | last_user=" + strconv.Quote(truncate(t, 60))
					}
				}
			}
		}
		return ""
	}
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// newBackendClient 构造后端 HTTP 客户端：
//  1. 优先环境变量代理（HTTP_PROXY/HTTPS_PROXY，与 Python httpx trust_env 一致）
//  2. 环境变量无代理时，回退读取系统代理（Windows 注册表 / GNOME dconf）
//  3. 都没有则直连。
func newBackendClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		Proxy: func(req *http.Request) (*url.URL, error) {
			if u, _ := http.ProxyFromEnvironment(req); u != nil {
				return u, nil
			}
			return systemProxy(req), nil
		},
	}
	return &http.Client{Transport: tr}
}

// directClient 直连客户端：完全忽略环境变量与系统代理。
// 用于国内可达的后端（copilot.tencent.com / mimo-server-cn.xiaomimimo.com），
// 避免本地代理软件关闭或切换网络后误走残留的 127.0.0.1:10808 等失效代理。
func directClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		Proxy:               nil,
	}
	return &http.Client{Transport: tr}
}

var sysProxyOnce sync.Once
var sysProxyEnable bool
var sysProxyServer string
var sysProxyOverride string

// systemProxy 取系统代理。取值在进程内缓存一次，改了代理要重启控制台才生效
// （与 Windows 上的历史行为一致）。
func systemProxy(req *http.Request) *url.URL {
	sysProxyOnce.Do(func() {
		sysProxyEnable, sysProxyServer, sysProxyOverride = loadSystemProxy()
	})
	if !sysProxyEnable || sysProxyServer == "" {
		return nil
	}
	host := req.URL.Hostname()
	if proxyOverrideMatch(sysProxyOverride, host) {
		return nil
	}
	addr := parseProxyServer(sysProxyServer, strings.ToLower(req.URL.Scheme))
	if addr == "" {
		return nil
	}
	return &url.URL{Scheme: "http", Host: addr}
}

// loadSystemProxy 按平台读系统代理设置：
//   Windows → HKCU 注册表 Internet Settings（reg.exe 只读查询）
//   其他    → GNOME dconf（gsettings）
//
// v2rayN 这类客户端在两端都是改「系统代理」而不是导出环境变量（本机实测 shell 与
// systemd 用户环境里都没有代理变量），所以只读 HTTP_PROXY 会让国际线路直连失败。
func loadSystemProxy() (bool, string, string) {
	if runtime.GOOS == "windows" {
		const key = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
		enable := strings.Contains(strings.ToLower(strings.TrimSpace(regQueryValue(key, "ProxyEnable"))), "0x1")
		return enable, strings.TrimSpace(regQueryValue(key, "ProxyServer")),
			strings.TrimSpace(regQueryValue(key, "ProxyOverride"))
	}
	return gnomeSystemProxy()
}

// gnomeSystemProxy 读 org.gnome.system.proxy。mode=manual 时按 http/https 两个子模式
// 取 host:port（https 留空则沿用 http，与 GNOME 自身行为一致）；mode=auto 是 PAC，
// 本程序不解 PAC，按「没有系统代理」处理。
//
// ignore-hosts 里的 CIDR（127.0.0.0/8 这类）现有匹配逻辑不认，但它只涉及回环地址，
// 而这个 transport 服务的是公网后端，不影响结果。
func gnomeSystemProxy() (bool, string, string) {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return false, "", "" // 非 GNOME 桌面（KDE/i3 等）：交给环境变量
	}
	if mode := strings.Trim(gsettingsGet("org.gnome.system.proxy", "mode"), "'\""); mode != "manual" {
		return false, "", ""
	}
	httpHost, httpPort := gnomeProxyEndpoint("http")
	httpsHost, httpsPort := gnomeProxyEndpoint("https")
	if httpsHost == "" {
		httpsHost, httpsPort = httpHost, httpPort
	}
	var parts []string
	if httpHost != "" && httpPort > 0 {
		parts = append(parts, fmt.Sprintf("http=%s:%d", httpHost, httpPort))
	}
	if httpsHost != "" && httpsPort > 0 {
		parts = append(parts, fmt.Sprintf("https=%s:%d", httpsHost, httpsPort))
	}
	if len(parts) == 0 {
		return false, "", ""
	}
	return true, strings.Join(parts, ";"), gsettingsIgnoreHosts()
}

func gnomeProxyEndpoint(scheme string) (string, int) {
	host := strings.Trim(gsettingsGet("org.gnome.system.proxy."+scheme, "host"), "'\"")
	port, _ := strconv.Atoi(gsettingsGet("org.gnome.system.proxy."+scheme, "port"))
	return host, port
}

// gsettingsIgnoreHosts 把 GNOME 的 ['a', 'b'] 列表转成内部用的 a;b 形式。
func gsettingsIgnoreHosts() string {
	raw := strings.TrimPrefix(gsettingsGet("org.gnome.system.proxy", "ignore-hosts"), "[")
	raw = strings.TrimSuffix(raw, "]")
	var out []string
	for _, it := range strings.Split(raw, ",") {
		if s := strings.Trim(strings.TrimSpace(it), "'\""); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ";")
}

func gsettingsGet(schema, key string) string {
	out, err := execCmd("gsettings", "get", schema, key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func regQueryValue(key, name string) string {
	out, err := execCmd("reg", "query", key, "/v", name).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.EqualFold(fields[0], name) {
			return strings.Join(fields[2:], " ")
		}
	}
	return ""
}

// parseProxyServer 处理 "host:port" 或 "http=host:port;https=host:port" 两种格式。
func parseProxyServer(s, scheme string) string {
	if !strings.Contains(s, "=") {
		return s
	}
	byScheme := map[string]string{}
	for _, kv := range strings.Split(s, ";") {
		kv = strings.TrimSpace(kv)
		if i := strings.Index(kv, "="); i > 0 {
			byScheme[strings.ToLower(strings.TrimSpace(kv[:i]))] = strings.TrimSpace(kv[i+1:])
		}
	}
	if v := byScheme[scheme]; v != "" {
		return v
	}
	return byScheme["http"]
}

// proxyOverrideMatch：匹配注册表 ProxyOverride（; 分隔，支持 <local>、*. 后缀、* 通配、精确主机）。
func proxyOverrideMatch(override, host string) bool {
	if override == "" || host == "" {
		return false
	}
	host = strings.ToLower(host)
	for _, pat := range strings.Split(override, ";") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if pat == "<local>" {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				return true
			}
			if host == "localhost" {
				return true
			}
			continue
		}
		lp := strings.ToLower(pat)
		if strings.HasPrefix(lp, "*.") {
			if strings.HasSuffix(host, lp[1:]) {
				return true
			}
			continue
		}
		if strings.Contains(lp, "*") {
			if ok, _ := filepath.Match(lp, host); ok {
				return true
			}
			continue
		}
		if lp == host {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// ---------- 上游 11140 风控的提示注入 ----------

// 2026-10-07 实测定论：WorkBuddy 上游的 403 {"code":11140,"msg":"request illegal"}
// 是【账号级风控】——同一请求（哪怕只是"你好"）在被标记账号上必挂，
// 换<凭证目录B>/<凭证目录C>立刻通过，与消息内容（含 GitHub 链接）无关。
// 但上游文案咬定「内容未通过安全审核」，用户会白白去改写提示词。
// 这里把人话提示追加进 displayMsg，客户端气泡会直接展示出来。

const riskControlHintZh = "\n\n[hub] 11140 多为账号级风控拦截（与本次内容无关）：到控制台点「换凭证并重启」换个账号即可恢复；若多个凭证都报此错才需考虑内容因素。"
const riskControlHintEn = "\n\n[hub] 11140 is usually account-level risk control, not a content problem: switch credential in the hub console and retry."

// annotateRiskControl 识别 11140 并就地往 displayMsg.zh / displayMsg.en 追加提示。
// 非 11140 的响应原样返回，行为不变。
func annotateRiskControl(status int, body []byte) []byte {
	if status != http.StatusForbidden || !strings.Contains(string(body), `"code":11140`) {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	dm, ok := m["displayMsg"].(map[string]any)
	if !ok {
		return body
	}
	if zh, ok := dm["zh"].(string); ok {
		dm["zh"] = zh + riskControlHintZh
	}
	if en, ok := dm["en"].(string); ok {
		dm["en"] = en + riskControlHintEn
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
