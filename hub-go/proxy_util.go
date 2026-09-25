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
//  2. 环境变量无代理时，回退读取 Windows 系统代理（注册表 Internet Settings）
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
			return windowsSystemProxy(req), nil
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

var winProxyOnce sync.Once
var winProxyEnable bool
var winProxyServer string
var winProxyOverride string

// windowsSystemProxy 读取 HKCU 注册表 Internet Settings（reg.exe 只读查询）。
func windowsSystemProxy(req *http.Request) *url.URL {
	winProxyOnce.Do(func() {
		key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
		enable := regQueryValue(key, "ProxyEnable")
		winProxyEnable = strings.Contains(strings.ToLower(strings.TrimSpace(enable)), "0x1")
		winProxyServer = strings.TrimSpace(regQueryValue(key, "ProxyServer"))
		winProxyOverride = strings.TrimSpace(regQueryValue(key, "ProxyOverride"))
	})
	if !winProxyEnable || winProxyServer == "" {
		return nil
	}
	host := req.URL.Hostname()
	if proxyOverrideMatch(winProxyOverride, host) {
		return nil
	}
	addr := parseProxyServer(winProxyServer, strings.ToLower(req.URL.Scheme))
	if addr == "" {
		return nil
	}
	return &url.URL{Scheme: "http", Host: addr}
}

func regQueryValue(key, name string) string {
	out, err := exec.Command("reg", "query", key, "/v", name).Output()
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
