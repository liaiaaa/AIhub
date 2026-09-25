package main

// 代理核心：OpenAI 兼容 /v1 接口 → 后端 /v2 转发。
// 移植自 converter.py / converter_cn.py，行为对齐：
//   - /health /v1/models /v1/chat/completions
//   - 后端只支持流式：始终以 stream=true 调后端，非流式由本地聚合
//   - developer 角色归一为 system；intl 首个非 system 时补 system
//   - 默认只记一行请求/响应摘要；--debug 才记完整体（Go 版经 --debug 由启动参数控制）

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var passthroughKeys = []string{
	"model", "messages", "tools", "tool_choice", "temperature",
	"max_tokens", "max_completion_tokens", "top_p", "stream",
	"stream_options", "stop", "presence_penalty", "frequency_penalty",
	"n", "response_format", "seed", "user", "reasoning_effort",
	"verbosity", "reasoning_summary",
}

type FileLogger struct {
	mu sync.Mutex
	f  *os.File
}

func NewFileLogger(path string) *FileLogger {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil
	}
	return &FileLogger{f: f}
}

func (l *FileLogger) Log(format string, args ...any) {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	l.f.WriteString(line)
}

// AuthProvider 抽象「怎么拿到后端请求头」。
// 国内/国际代理用 CredentialManager（读 JSON 凭证 + 刷新 accessToken）；
// MiMo 用 MimoAuth（小米账号 SSO 换 serviceToken）。两者认证机制完全不同，
// 这里只统一它们对外的样子。
type AuthProvider interface {
	Headers() (map[string]string, error)
	Summary() (map[string]any, error)
}

// Proxy 一个本地代理实例。零值不可用，须用 NewDomProxy/NewIntlProxy/NewMimoProxy。
type Proxy struct {
	Name          string
	Kind          string // "cred"（凭证文件）或 "mimo"（小米 SSO）
	Port          int
	Backend       string
	ChatPath      string // 后端聊天路径，默认 /v2/chat/completions
	DefaultDomain string
	UserAgent     string
	DefaultModel  string
	Models        []string
	Aliases       map[string]string
	AuthDir       string // intl：凭证目录；dom：""
	AuthFileName  string
	PrependSystem bool // intl 需要首条 system
	Owner         string
	LogPath       string
	DirectOnly    bool // 直连后端，不读系统代理（国内 / 小米服务器，避免依赖代理软件）

	mu     sync.Mutex
	auth   AuthProvider
	log    *FileLogger
	client *http.Client
	server *http.Server
	ln     net.Listener
}

// chatPath 返回后端聊天接口路径。
func (p *Proxy) chatPath() string {
	if p.ChatPath != "" {
		return p.ChatPath
	}
	return "/v2/chat/completions"
}

func (p *Proxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

// Start 绑定端口并开始服务。
// 关键点：只有【本实例自己的 listener】绑定成功才算启动成功。
// 早期实现只探测端口是否可连接，端口被别的进程占用时会误报「已启动」。
func (p *Proxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	auth, msg := p.buildAuth()
	if auth == nil {
		return false, msg
	}
	log := NewFileLogger(dataPath(p.LogPath))

	// 先自己绑定端口，绑定失败就如实报错（而不是靠端口探测猜）。
	// 端口被占时先清扫再重试，最多重试 3 次、每次间隔 500ms，
	// 以应对上一次会话残留的 TIME_WAIT / 进程还没完全退出等瞬时冲突。
	addr := fmt.Sprintf("127.0.0.1:%d", p.Port)
	var ln net.Listener
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		// 清扫占用者再重试
		stopPort(p.Port)
	}
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动（已重试 %d 次）：%v", p.Port, 3, err)
	}

	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 20 * time.Second,
	}
	p.mu.Lock()
	p.auth = auth
	p.log = log
	if p.DirectOnly {
		p.client = directClient()
	} else {
		p.client = newBackendClient()
	}
	p.server = srv
	p.ln = ln
	p.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Log("serve error: %v", err)
		}
	}()
	log.Log("==== proxy 启动 ====")
	return true, "已启动"
}

// Stop 关闭本实例的监听并释放端口。
// 关键点：只关自己的 listener，绝不按端口 PID 去 taskkill。
// 早期实现会在端口仍被占用时 taskkill 该端口上的进程——而代理是进程内的，
// 那个 PID 往往就是 hub 自己，等于自杀；也会误杀占用该端口的外部程序。
func (p *Proxy) Stop() bool {
	p.mu.Lock()
	srv, ln := p.server, p.ln
	p.server, p.ln = nil, nil
	p.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = srv.Shutdown(ctx) // 先给在途请求一点时间收尾
		cancel()
		_ = srv.Close() // 超时则强制断开，保证端口一定释放
	}
	if ln != nil {
		_ = ln.Close()
	}
	// 只做端口确认，不再杀进程。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !portListening(p.Port) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !portListening(p.Port)
}

// buildAuth 按代理类型构造认证提供者。返回 nil 时 msg 是给用户看的原因。
func (p *Proxy) buildAuth() (AuthProvider, string) {
	if p.Kind == "mimo" {
		// MiMo 不读 JSON 凭证，而是从桌面端的登录 cookie 换 serviceToken。
		a := NewMimoAuth()
		if err := a.Probe(); err != nil {
			return nil, err.Error()
		}
		return a, ""
	}
	af := p.AuthFilePath()
	if af == "" || !fileExists(af) {
		return nil, "未找到登录凭据文件，请先在桌面端登录"
	}
	cred := NewCredentialManager(af, p.Backend, p.DefaultDomain, p.UserAgent)
	if _, err := cred.Summary(); err != nil {
		return nil, "读取凭据失败：" + err.Error()
	}
	return cred, ""
}

func (p *Proxy) AuthFilePath() string {
	if p.Kind == "mimo" {
		return mimoCookiePath() // MiMo 没有 JSON 凭证，认证数据在桌面端 cookie 库里
	}
	if p.AuthDir != "" {
		exact := filepath.Join(p.AuthDir, p.AuthFileName)
		if fileExists(exact) {
			return exact
		}
		// 文件名不固定（如「<凭证目录C>」的 ai-<uuid>.info）：扫目录回退。
		// 优先标准名，其次按文件名字典序取第一个，保证结果稳定。
		if m, _ := filepath.Glob(filepath.Join(p.AuthDir, "*.info")); len(m) > 0 {
			sort.Strings(m)
			for _, e := range m {
				if filepath.Base(e) == p.AuthFileName {
					return e
				}
			}
			return m[0]
		}
		return exact // 保持原行为：返回预期路径，让上层报「未找到」
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		d := filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth")
		pref := filepath.Join(d, p.AuthFileName)
		if fileExists(pref) {
			return pref
		}
		entries, _ := filepath.Glob(filepath.Join(d, "*.info"))
		for _, e := range entries {
			base := filepath.Base(e)
			if p.AuthFileName == "workbuddy-desktop.info" && strings.HasSuffix(base, "-ai.info") {
				continue
			}
			return e
		}
	}
	return ""
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		p.handleHealth(w, r)
	case "/v1/models":
		p.handleModels(w, r)
	case "/v1/chat/completions":
		p.handleChat(w, r)
	default:
		http.NotFound(w, r)
	}
}

// runtime 一次性取出当前认证/日志/客户端，避免各处直接读字段产生数据竞争。
func (p *Proxy) runtime() (AuthProvider, *FileLogger, *http.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auth, p.log, p.client
}

func (p *Proxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	auth, _, _ := p.runtime()
	info := map[string]any{"status": "ok", "backend": p.Backend, "auth_file": p.AuthFilePath()}
	if auth != nil {
		if s, err := auth.Summary(); err == nil {
			info["credential"] = s
		} else {
			info["credential_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]any, 0, len(p.Models))
	for _, m := range p.Models {
		data = append(data, map[string]any{"id": m, "object": "model", "created": 1700000000, "owned_by": p.Owner})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (p *Proxy) handleChat(w http.ResponseWriter, r *http.Request) {
	auth, logger, client := p.runtime()

	if auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "未找到登录凭据，请先在桌面端登录", "type": "auth_error"},
		})
		return
	}
	var raw map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad json: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	msgs, _ := raw["messages"].([]any)
	if len(msgs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "messages is required", "type": "invalid_request_error"},
		})
		return
	}
	clientStream, _ := raw["stream"].(bool)

	body := map[string]any{}
	for _, k := range passthroughKeys {
		if v, ok := raw[k]; ok {
			body[k] = v
		}
	}
	body["model"] = p.mapModel(fmt.Sprintf("%v", raw["model"]))
	body["messages"] = p.normalizeMessages(msgs)
	body["stream"] = true
	if _, ok := body["stream_options"]; !ok {
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	rid := randHex(4)
	modelName := fmt.Sprintf("%v", body["model"])
	logger.Log("[%s] ▶ REQUEST %s | stream=%v | msgs=%d%s", rid, modelName, clientStream, len(msgs),
		lastUserSuffix(msgs))

	headers, err := auth.Headers()
	if err != nil {
		logger.Log("[%s] ✗ 凭据错误 | %v", rid, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "auth error: " + err.Error(), "type": "auth_error"},
		})
		return
	}
	payload, _ := json.Marshal(body)
	url := p.Backend + p.chatPath()
	t0 := time.Now()

	if clientStream {
		p.streamToClient(w, r, auth, client, logger, url, headers, payload, t0, rid)
		return
	}
	p.aggregateToClient(w, r, auth, client, logger, url, headers, payload, t0, rid)
}

func (p *Proxy) mapModel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return p.DefaultModel
	}
	if v, ok := p.Aliases[name]; ok {
		return v
	}
	return name
}

// normalizeMessages：developer→system；intl 在首条非 system 时补一条 system。
func (p *Proxy) normalizeMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		if role, _ := mm["role"].(string); role == "developer" {
			nm := cloneMap(mm)
			nm["role"] = "system"
			out = append(out, nm)
			changed = true
			continue
		}
		out = append(out, m)
	}
	if p.PrependSystem {
		firstRole := ""
		if len(out) > 0 {
			if fm, ok := out[0].(map[string]any); ok {
				firstRole, _ = fm["role"].(string)
			}
		}
		if len(out) == 0 || firstRole != "system" {
			out = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, out...)
		}
		return out
	}
	if !changed {
		return msgs
	}
	return out
}

// authInvalidator 认证提供者可选的「作废缓存」能力。
// MiMo 的 serviceToken 有效期不由我们掌握，服务端可能提前判失效。
type authInvalidator interface{ Invalidate() }

// doUpstream 发起后端请求；返回 401 且认证支持作废时，换票后重试一次。
func (p *Proxy) doUpstream(ctx context.Context, client *http.Client, auth AuthProvider, url string, headers map[string]string, payload []byte) (*http.Response, error) {
	attempt := func(h map[string]string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		for k, v := range h {
			req.Header.Set(k, v)
		}
		return client.Do(req)
	}
	resp, err := attempt(headers)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	inv, ok := auth.(authInvalidator)
	if !ok {
		return resp, nil // 凭证文件类认证：401 就是真失败，交给上层报错
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	inv.Invalidate()
	h2, err := auth.Headers()
	if err != nil {
		return nil, err
	}
	return attempt(h2)
}

func (p *Proxy) streamToClient(w http.ResponseWriter, r *http.Request, auth AuthProvider, client *http.Client, logger *FileLogger, url string, headers map[string]string, payload []byte, t0 time.Time, rid string) {
	resp, err := p.doUpstream(r.Context(), client, auth, url, headers, payload)
	if err != nil {
		logger.Log("[%s] ✗ 网络错误 | %v", rid, err)
		sseError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		logger.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		sseError(w, resp.StatusCode, truncate(string(b), 500))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 边转发边 flush：否则 SSE 会被 http 的写缓冲攒住，客户端看起来像卡住。
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break // 客户端断开（含控制台点「停止」）
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	logger.Log("[%s] ◀ RESPONSE | %.1fs | stream", rid, time.Since(t0).Seconds())
}

func (p *Proxy) aggregateToClient(w http.ResponseWriter, r *http.Request, auth AuthProvider, client *http.Client, logger *FileLogger, url string, headers map[string]string, payload []byte, t0 time.Time, rid string) {
	// 用请求上下文：客户端断开时能及时取消上游，不再空转到 300 秒。
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	resp, err := p.doUpstream(ctx, client, auth, url, headers, payload)
	if err != nil {
		logger.Log("[%s] ✗ 网络错误 | %v", rid, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"},
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		logger.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		var detail any
		if json.Unmarshal(b, &detail) == nil {
			writeJSON(w, resp.StatusCode, detail)
		} else {
			writeJSON(w, resp.StatusCode, map[string]any{
				"error": map[string]any{"message": string(b), "type": "upstream_error", "code": resp.StatusCode},
			})
		}
		return
	}
	col := collectStream(resp.Body)
	elapsed := time.Since(t0)
	tokens := "?"
	if col.usage != nil {
		if v, ok := col.usage["total_tokens"].(json.Number); ok {
			tokens = v.String()
		}
	}
	logger.Log("[%s] ◀ RESPONSE | %.1fs | finish=%s | tokens=%s", rid, elapsed.Seconds(), col.finish, tokens)
	writeJSON(w, http.StatusOK, col.build())
}




