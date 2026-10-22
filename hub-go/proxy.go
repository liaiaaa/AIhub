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

	// ConversationHeaders：给上游补「官方客户端」的会话四头，见 withConversationHeaders。
	// 2026-10-16 起 intl 必需（缺了整条线路全废）。
	ConversationHeaders bool

	// OfficialChatClient：对话请求**整体**伪装成官方桌面端内置 CLI。
	// 2026-10-20 23:00 起腾讯给 WorkBuddy 对话网关加了第三方客户端渠道校验
	// （400 code=11128 Illegal API invocation from an unapproved channel），
	// 只补会话四头不够，还要 UA、X-Agent-*、X-IDE-*、traceparent 等一整套。
	// 只在 dom 置位：国际版此前已靠 ConversationHeaders 解决，不动它。
	OfficialChatClient bool

	// ThinkingDefault：客户端没指定思考档位时补的默认档（见 default_thinking.go）。
	// 空串 = 不注入，完全透传。
	ThinkingDefault string

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
		return nil, fmt.Sprintf("未找到登录凭据文件（预期位置：%s）。桌面端登录后可把它的 auth 目录整体拷到数据根，或用 WORKBUDDY_AUTH_DIR 指到放凭据的目录", af)
	}
	cred := NewCredentialManager(af, p.Backend, p.DefaultDomain, p.UserAgent)
	if _, err := cred.Summary(); err != nil {
		return nil, "读取凭据失败：" + err.Error()
	}
	return cred, ""
}

// pickAuthFile 在一个目录里挑凭据文件：先要标准名，再退到目录内任意 *.info。
// 国内线路绝不捡走国际线路那份 -ai.info（两套后端、两套 token）。
func (p *Proxy) pickAuthFile(dir string) string {
	if dir == "" {
		return ""
	}
	exact := filepath.Join(dir, p.AuthFileName)
	if fileExists(exact) {
		return exact
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.info"))
	if len(m) == 0 {
		return ""
	}
	sort.Strings(m)
	for _, e := range m {
		base := filepath.Base(e)
		if base == p.AuthFileName {
			return e
		}
		if p.AuthFileName == "workbuddy-desktop.info" && strings.HasSuffix(base, "-ai.info") {
			continue
		}
		return e
	}
	return ""
}

func (p *Proxy) AuthFilePath() string {
	if p.Kind == "mimo" {
		return mimoCookiePath() // MiMo 没有 JSON 凭证，认证数据在桌面端 cookie 库里
	}
	if p.AuthDir != "" {
		if got := p.pickAuthFile(p.AuthDir); got != "" {
			return got
		}
		return filepath.Join(p.AuthDir, p.AuthFileName) // 保持原行为：返回预期路径，让上层报「未找到」
	}
	// 桌面端安装目录（Windows 才有 %LOCALAPPDATA%）。
	domAuthRel := []string{"CodeBuddyExtension", "Data", "Public", "auth"}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		if got := p.pickAuthFile(filepath.Join(local, filepath.Join(domAuthRel...))); got != "" {
			return got
		}
	}
	// 非 Windows（或不想装桌面端）时的兜底：环境变量指定目录 > 数据根下的镜像目录 > 数据根本身。
	// 镜像目录的规则是「把 Windows 上那个目录原样拷过来」：
	//   %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth  →  <数据根>/CodeBuddyExtension/Data/Public/auth
	// 只拷一个文件也行：<数据根>/workbuddy-desktop.info
	for _, dir := range []string{
		os.Getenv("WORKBUDDY_AUTH_DIR"),
		dataPath(domAuthRel...),
		hubRoot,
	} {
		if got := p.pickAuthFile(dir); got != "" {
			return got
		}
	}
	// 数据根下的子目录：用户习惯把凭据放成「国内workbuddy凭证」这类命名的目录
	// （和「cred-a / cred-c」给国际线路用是同一种组织方式）。
	// 这里只认**标准文件名**，不走任意 *.info 回退，避免误捡无关目录里的杂文件。
	if entries, err := os.ReadDir(hubRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			candidate := filepath.Join(hubRoot, e.Name(), p.AuthFileName)
			if fileExists(candidate) {
				return candidate
			}
		}
	}
	return dataPath(p.AuthFileName)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		p.handleHealth(w, r)
	case "/v1/models":
		p.handleModels(w, r)
	case "/v1/chat/completions":
		p.handleChat(w, r)
	case "/v1/responses":
		p.handleResponses(w, r)
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
	// codex 的 models manager 带 client_version 查询参数，且只认自家的
	// ModelsEndpointResponse 格式；dsh 等标准 OpenAI 客户端无该参数，维持原格式。
	if r.URL.Query().Get("client_version") != "" {
		writeJSON(w, http.StatusOK, codexModelsResponse(p.Models))
		return
	}
	data := make([]map[string]any, 0, len(p.Models))
	for _, m := range p.Models {
		entry := map[string]any{"id": m, "object": "model", "created": 1700000000, "owned_by": p.Owner}
		// dsh 等客户端读 name 作为悬停提示/显示名；无倍率数据则不写，回退用 id
		if name := ratioName(p.Name, m); name != "" {
			entry["name"] = name
		}
		data = append(data, entry)
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
	// 2026-10-21：dom 开了官方客户端伪装时，system 里的第三方客户端签名会被
	// 上游渠道校验识别（11128），转发前替换掉。见 sanitizeHarnessPrompt。
	if p.OfficialChatClient {
		body["messages"] = sanitizeHarnessMessages(body["messages"].([]any))
	}
	body["stream"] = true
	if _, ok := body["stream_options"]; !ok {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	// 客户端没指定思考档位 → 用该线路的默认档（2026-10-08 用户要求：默认最高）。
	applyDefaultThinking(body, raw, msgs, p.ThinkingDefault)

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

// normalizeMessages：developer→system；剔除空内容 system 消息（DSH 切模型时
// 会插入 {"role":"system","content":[]}，部分上游对此报 provider_error，
// 2026-09-26 在 Qoder 网关实测复现）；intl 在首条非 system 时补一条 system。
func (p *Proxy) normalizeMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "developer" {
			nm := cloneMap(mm)
			nm["role"] = "system"
			out = append(out, nm)
			changed = true
			continue
		}
		if role == "system" && qoderMsgContentEmpty(mm["content"]) {
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

// withConversationHeaders 补齐上游「通路校验」要的会话四头。
//
// 2026-10-16 起 www.workbuddy.ai 对 /v2/chat/completions 开了一道渠道校验：缺头就
//
//	400 {"code":11128,"msg":"conversation id missing",
//	     "displayMsg":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。"}}
//
// 流式请求更阴：HTTP 200 + 一个 data:{"error":{...}} 帧，正文为空 —— 上层看着像成功。
//
// 四个值都是客户端自造的随机 id，**不需要先向服务端申请会话**（实测随机值即通）。
// 形状抄自官方客户端：conversation / conversationRequestId 是 32 位裸 hex，
// messageID / requestID 是带连字符的 UUID —— 两种都见过，别混。
// 头名与取值来自 app.asar 里的 Yr()（见 docs/2026-10-16-intl通路校验.md）。
//
// 只在 ConversationHeaders 置位的信源启用：dom 线路实测不需要，贸然加会改变
// 一个能正常工作的东西的请求形状，不做这种「顺手统一」。
func (p *Proxy) withConversationHeaders(h map[string]string) map[string]string {
	if p.OfficialChatClient {
		return officialChatHeaders(h)
	}
	if !p.ConversationHeaders {
		return h
	}
	out := make(map[string]string, len(h)+4)
	for k, v := range h {
		out[k] = v
	}
	out["X-Conversation-ID"] = randHex(16)
	out["X-Conversation-Request-ID"] = randHex(16)
	out["X-Conversation-Message-ID"] = randUUID()
	out["X-Request-ID"] = randUUID()
	return out
}

// 官方国内版桌面端 / 内置 CLI 的版本号。
// 依据 yreidev/workbuddy2api-panel docs/official-client.md：桌面端
// WorkBuddy 5.8.3（macOS arm64）、内置 CLI @tencent-ai/codebuddy-code 2.161.0，
// 网关的 UA 报的就是安装包内置版本。上游若按版本号判渠道，这两个值要对得上。
const (
	officialWBVersion  = "5.8.3"
	officialCLIVersion = "2.161.0"
)

// officialChatHeaders 把对话请求整体伪装成官方桌面端内置 CLI 发出的。
//
// 2026-10-20 23:00 腾讯给 WorkBuddy 对话网关加了第三方客户端渠道校验，症状：
//
//	400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel",
//	     "displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的"}}
//
// 与 2026-10-16 国际版那次（只缺会话四头）不同，这次是整套请求指纹校验，
// 只补四个头不够。逐项对齐依据见 docs/official-client.md：
//
//	UA         WorkBuddy/<版本> <应用名>/<版本> CLI/<cli>
//	公共头     Accept "application/json, text/plain, */*"、X-Requested-With、
//	           X-Request-ID（uuidv7 去连字符）、X-Product: SaaS、
//	           Accept-Encoding "gzip, compress, deflate, br"
//	对话专项   X-Agent-Intent: craft、X-Agent-Type: main、
//	           X-Agent-Purpose: conversation、X-IDE-Name/Type: WorkBuddy、
//	           X-IDE-Version、X-Private-Data: "true"（值带引号）
//	会话 id    X-Conversation-ID（uuidv7 带连字符 36 位）、
//	           X-Conversation-Request-ID（去连字符 32 位）、
//	           X-Conversation-Message-ID 与 X-Request-ID（同一消息 id）
//	链路头     traceparent、b3、X-B3-TraceId/SpanId/Sampled
//
// 官方明确**不发** Origin、Referer、X-CodeBuddy-Request、X-Machine-ID、
// X-Session-ID，所以这里也不补（CredentialManager 本来就没发）。
func officialChatHeaders(h map[string]string) map[string]string {
	now := time.Now()
	convID := randUUIDv7(now)          // 会话 id：带连字符
	turnID := uuidv7Hex(now)           // 轮次 id：去连字符
	msgID := uuidv7Hex(now)            // 消息 id：与 X-Request-ID 同值
	traceID := uuidv7Hex(now)          // 链路 trace id
	spanID := randHex(8)               // 16 位十六进制 span

	out := make(map[string]string, len(h)+22)
	for k, v := range h {
		out[k] = v
	}
	out["User-Agent"] = "WorkBuddy/" + officialWBVersion + " WorkBuddy/" + officialWBVersion + " CLI/" + officialCLIVersion
	out["Accept"] = "application/json, text/plain, */*"
	out["Content-Type"] = "application/json"
	out["Accept-Encoding"] = "gzip, compress, deflate, br"
	out["X-Requested-With"] = "XMLHttpRequest"
	out["X-Request-ID"] = msgID
	out["X-Product"] = "SaaS"

	out["X-Agent-Intent"] = "craft"
	out["X-Agent-Type"] = "main"
	out["X-Agent-Purpose"] = "conversation"
	out["X-IDE-Name"] = "WorkBuddy"
	out["X-IDE-Type"] = "WorkBuddy"
	out["X-IDE-Version"] = officialWBVersion
	out["X-Private-Data"] = `"true"`

	out["X-Conversation-ID"] = convID
	out["X-Conversation-Request-ID"] = turnID
	out["X-Conversation-Message-ID"] = msgID

	out["traceparent"] = "00-" + traceID + "-" + spanID + "-01"
	out["b3"] = traceID + "-" + spanID + "-1-"
	out["X-B3-TraceId"] = traceID
	out["X-B3-SpanId"] = spanID
	out["X-B3-Sampled"] = "1"
	return out
}

// doUpstream 发起后端请求；返回 401 且认证支持作废时，换票后重试一次。
func (p *Proxy) doUpstream(ctx context.Context, client *http.Client, auth AuthProvider, url string, headers map[string]string, payload []byte) (*http.Response, error) {
	attempt := func(h map[string]string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		for k, v := range p.withConversationHeaders(h) {
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
	// 走 429 感知重试：瞬时限流在 hub 内部退避吸收（见 proxy_retry429.go）
	resp, err := p.doUpstreamRetry429(r.Context(), client, auth, url, headers, payload, logger, rid)
	if err != nil {
		logger.Log("[%s] ✗ 网络错误 | %v", rid, err)
		sseError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		logger.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		// 2026-10-21 排查 11128 渠道校验用：命中时把发往上游的完整信封落盘，
		// 之前只能靠「构造类似请求」猜触发条件，猜了 4 轮都不中（body 大小、
		// ACP 标记、system 长度、并发全排除），真正需要的是失败样本本身。
		if resp.StatusCode == http.StatusBadRequest && bytes.Contains(b, []byte("unapproved channel")) {
			// 单份上限：超过这个体积基本是无关的超长上下文，落盘只会挤掉
			// 真正小的失败样本（排查用，不是留档用）。
			const domDumpMaxBytes = 8 << 20
			if len(payload) <= domDumpMaxBytes {
				dumpPath := dataPath("dom-dump-" + rid + "-upstream.json")
				if err := os.WriteFile(dumpPath, payload, 0600); err == nil {
					logger.Log("[%s] ⚠ 已 dump 渠道拦截载荷: %s (%dKB)", rid, dumpPath, len(payload)/1024)
					// 2026-10-21 开源审查补：此前 dom 这条路径没有清理，
					// 每命中一次 11128 就留一份整份对话，永不删。复用 qoder
					// 那边抽出的通用清理（保留最近 3 份，AIHUB_KEEP_DUMPS=1 可关）。
					pruneDumpFiles("dom-dump-*.json", domDumpKeep, "AIHUB_KEEP_DUMPS", logger, "dom")
				}
			}
		}
		recordRateLimitFromBody(auth, payload, b)
		b = annotateRiskControl(resp.StatusCode, b)
		sseError(w, resp.StatusCode, truncate(string(b), 1200))
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
			// io.EOF 是正常的流结束；其它错误（连接重置、上游超时）此前被
			// 静默 break —— 客户端收到半截流，日志里却只有一行「◀ RESPONSE」，
			// 完全看不出发生过中断。2026-10-21 开源审查补记日志。
			if rerr != io.EOF {
				logger.Log("[%s] ✗ 上游流中断 | %v", rid, rerr)
			}
			break
		}
	}
	logger.Log("[%s] ◀ RESPONSE | %.1fs | stream", rid, time.Since(t0).Seconds())
}

func (p *Proxy) aggregateToClient(w http.ResponseWriter, r *http.Request, auth AuthProvider, client *http.Client, logger *FileLogger, url string, headers map[string]string, payload []byte, t0 time.Time, rid string) {
	// 用请求上下文：客户端断开时能及时取消上游，不再空转到 300 秒。
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	resp, err := p.doUpstreamRetry429(ctx, client, auth, url, headers, payload, logger, rid)
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
		recordRateLimitFromBody(auth, payload, b)
		b = annotateRiskControl(resp.StatusCode, b)
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
	if col.errMsg != "" {
		// 上游 HTTP 200 但流里是错误帧：改成 502 交出去，不要让客户端
		// 拿到「成功 + 空内容」（用户会以为模型不输出，实际是上游报错）。
		logger.Log("[%s] ✗ 上游流内报错 | %s", rid, truncate(col.errMsg, 200))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"message": "上游在流式响应里返回错误：" + col.errMsg,
				"type":    "upstream_error",
			},
		})
		return
	}
	logger.Log("[%s] ◀ RESPONSE | %.1fs | finish=%s | tokens=%s", rid, elapsed.Seconds(), col.finish, tokens)
	writeJSON(w, http.StatusOK, col.build())
}


// domDumpKeep dom 线路失败样本（渠道拦截载荷）保留份数。
// 只留最近几份够对比定位；数据根要在开源后随仓库/文档可见，明文对话历史
// 不能无限堆（此前每命中一次 11128 就留一份整份对话，没有任何清理）。
const domDumpKeep = 3
