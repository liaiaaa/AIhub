package main

// QoderProxy 服务：启动/停止/HTTP 路由/模型解析
// 由 proxy_qoder.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 代理服务 ----------

const (
	// qoderMaxConcurrentDefault 上游并发上限的默认值 —— 0 = 不限制。
	//
	// 2026-10-28 用户要求撤掉本地并发限制：「别设置本地并发限制了，太妨碍我的使用了」。
	// 此前是 2，dsh 多开会话时第 3 个直接吃本地 429，用起来像断线。
	// 现在默认完全放行，想重新设上限用 QODER_MAX_CONCURRENT=N 或数据根
	// qoder_max_concurrent.txt（每请求现读，改完不用重启）。
	qoderMaxConcurrentDefault = 0

	// qoderMaxConcurrentEnvFile / qoderMaxConcurrentEnv 上限的配置来源。
	qoderMaxConcurrentFile = "qoder_max_concurrent.txt"
	qoderMaxConcurrentEnv  = "QODER_MAX_CONCURRENT"

	// qoderMaxConcurrentCeil 上限的硬封顶，防止手滑写成 10000 把上游打爆。
	qoderMaxConcurrentCeil = 64

	// qoderMinGapDefault 相邻上游请求最小间隔（拟人化，默认 800ms）+ 0~400ms 抖动。
	// 官方客户端是人工点触发的，不会有毫秒级连续请求；固定间隔本身也是特征。
	// 0 = 不等待。QODER_MIN_GAP_MS=N 或数据根 qoder_min_gap.txt 可改。
	qoderMinGapDefault = 800
	qoderMinGapJitter  = 400

	qoderMinGapFile = "qoder_min_gap.txt"
	qoderMinGapEnv  = "QODER_MIN_GAP_MS"
	// qoderMinGapCeil 间隔的硬封顶，防止写错把请求卡死。
	qoderMinGapCeil = 10000
)

type QoderProxy struct {
	Port     int
	inferURL string // 上游推理接口（测试可注入 httptest 服务）
	auth     *QoderAuth
	log      *FileLogger
	client   *http.Client

	// 模型目录：catalog 为权威清单，aliases 为「别名 → 真实 key」映射。
	// 由 qoder_models.go 提供，启动后异步刷新。catalogMu 保护二者。
	catalogMu sync.RWMutex
	catalog   []qoderModelEntry
	aliases   map[string]string

	// 上游并发闸门 + 拟人间隔。
	// inflight 是当前在途请求数（qoderMaxConcurrentDefault=0 时只作统计，
	// 不做拦截）；用计数而不是 chan 是为了让上限可以每请求现读、随时改。
	inflightMu sync.Mutex
	inflight   int
	lastReqMu  sync.Mutex
	lastReq   time.Time

	// 会话租约：同一段对话在 TTL 内复用 session_id/request_set_id/business.id
	sessionMu    sync.Mutex
	sessionCache map[string]*qoderSessionLease

	mu     sync.Mutex
	ln     net.Listener
	server *http.Server
}

func NewQoderProxy() *QoderProxy {
	catalog := defaultQoderCatalog() // 内置快照已含 context_config（见 defaultQoderCatalog）
	return &QoderProxy{
		Port:         QoderPort,
		inferURL:     QoderInferURL,
		catalog:      catalog,
		aliases:      buildQoderAliases(catalog),
		sessionCache: make(map[string]*qoderSessionLease),
	}
}

// catalogSnapshot 返回当前目录副本。
func (p *QoderProxy) catalogSnapshot() []qoderModelEntry {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	out := make([]qoderModelEntry, len(p.catalog))
	copy(out, p.catalog)
	return out
}

// setCatalog 热替换目录与别名表（动态刷新用）。
func (p *QoderProxy) setCatalog(entries []qoderModelEntry) {
	if len(entries) == 0 {
		return
	}
	aliases := buildQoderAliases(entries)
	p.catalogMu.Lock()
	p.catalog = entries
	p.aliases = aliases
	p.catalogMu.Unlock()
}

// aliasLookup 查别名表。
func (p *QoderProxy) aliasLookup(name string) (string, bool) {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	if p.aliases == nil {
		return "", false
	}
	key, ok := p.aliases[qoderNormalizeAlias(name)]
	return key, ok
}

func (p *QoderProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

func (p *QoderProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	if p.auth == nil {
		a, err := NewQoderAuth()
		if err != nil {
			return false, err.Error()
		}
		p.auth = a
	}
	if err := p.auth.Reload(); err != nil {
		return false, "Qoder 凭据加载失败：" + err.Error()
	}
	p.log = NewFileLogger(dataPath("converter-qoder.log"))
	// gateway.qoder.com.cn 国内（阿里云）可达：直连，不读系统/环境代理。
	// 推理请求在 Sign 里显式带 Accept-Encoding: identity，Go 不会自动加
	// gzip/解压；万一上游仍回 gzip，由 qoderMaybeDecompress 兜底。
	p.client = directClient()

	ln, err := listenLinePort(p.Port)
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", p.Port, err)
	}

	srv := newLineServer(p)
	p.mu.Lock()
	p.server = srv
	p.ln = ln
	p.mu.Unlock()

	name, _, _, _ := p.auth.Snapshot()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			if p.log != nil {
				p.log.Log("serve error: %v", err)
			}
		}
	}()
	// 异步拉取官方模型目录（失败则沿用内置快照），不阻塞启动路径。
	go p.refreshCatalog()
	if p.log != nil {
		p.log.Log("==== qoder proxy 启动 ====")
	}
	logHub("qoder proxy started on 127.0.0.1:%d (user %s)", p.Port, name)
	return true, "已启动（Qoder CN · " + name + "）"
}

func (p *QoderProxy) Stop() bool {
	p.mu.Lock()
	srv, ln := p.server, p.ln
	p.server, p.ln = nil, nil
	p.mu.Unlock()

	closeLineServer(srv, ln)
	return waitPortFree(p.Port, lineShutdownGrace)
}

func (p *QoderProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}
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

func (p *QoderProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{"status": "ok", "backend": QoderInferURL, "proxy_port": p.Port}
	if p.auth != nil {
		name, uid, tokenHead, expiresAt := p.auth.Snapshot()
		info["credential"] = map[string]any{
			"user": name, "uid": uid, "token_head": tokenHead, "expires_at": expiresAt,
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// handleModels 每个模型只暴露**一条**记录：id 用官方显示名（如 GLM-5.3）。
//
// 为什么不再同时列出真实 key：同时列两条会让客户端（Cherry Studio 等）出现
// 成对的重复项（key 条目带 display_name 字段，客户端优先渲染它，两条看起来
// 一模一样）。显示名是唯一 id，请求时由别名表映射回真实 key；
// 真实 key 依然可以直接调用（别名表里 key→key 始终存在），只是不再单独列出。
//
// 兜底：若某模型没有显示名（或与其他模型重名），则用真实 key 作为 id，
// 保证 id 永不重复、永不缺失。
func (p *QoderProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	catalog := p.catalogSnapshot()
	data := make([]map[string]any, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		id := m.DisplayName
		if id == "" {
			id = m.Key
		}
		if seen[id] {
			continue // 动态目录万一重名：先到先得，真实 key 仍可直接调用
		}
		seen[id] = true
		entry := map[string]any{
			"id": id, "object": "model", "created": 1700000000, "owned_by": m.Vendor,
		}
		// dsh 等客户端读 name：官方显示名 + 计费倍率（目录 price_factor）
		if m.IsFree {
			entry["name"] = id + " · 免费"
		} else if m.PriceFactor > 0 {
			entry["name"] = id + " · " + ratioLabel(m.PriceFactor)
		}
		if id != m.Key {
			entry["resolves_to"] = m.Key // 透出真实 key，便于排查
		}
		// 服务端拉取的思考强度档（thinking_config），客户端可据此渲染 effort 选项
		if len(m.Efforts) > 0 {
			entry["efforts"] = m.Efforts
			if m.DefaultEffort != "" {
				entry["default_effort"] = m.DefaultEffort
			}
		}
		// 能力元数据：视觉输入 / 上下文与输出上限（代理自身也按此钳制 max_tokens）
		entry["vision"] = m.IsVision
		if m.MaxInputTokens > 0 {
			entry["max_input_tokens"] = m.MaxInputTokens
		}
		if m.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = m.MaxOutputTokens
		} else {
			entry["max_output_tokens"] = qoderChatMaxTokens
		}
		if len(m.ContextWindows) > 0 {
			entry["context_windows"] = m.ContextWindows
		}
		data = append(data, entry)
	}
	defKey := qoderDefaultKey(catalog)
	defID := defKey
	for _, m := range catalog {
		if m.Key == defKey && m.DisplayName != "" {
			defID = m.DisplayName // 与列表里的 id 保持一致
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":      "list",
		"data":        data,
		"default":     defID,
		"default_key": defKey,
	})
}

// qoderSSEWrapper 上游 SSE 包装帧。
type qoderSSEWrapper struct {
	Headers         map[string]any `json:"headers"`
	Body            string         `json:"body"`
	StatusCodeValue int            `json:"statusCodeValue"`
	StatusCode      string         `json:"statusCode"`
}

// resolveModel 把客户端传来的模型名归一为上游真实 key。
//   - 命中别名表（真实 key / 官方显示名 / 人工别名，大小写与下划线不敏感）→ 真实 key
//   - 未命中 → 原样透传（保留对未收录新模型的兼容），并记 WARN 便于发现拼写错误
func (p *QoderProxy) resolveModel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return QoderDefaultMd
	}
	if key, ok := p.aliasLookup(name); ok {
		return key
	}
	if p.log != nil {
		p.log.Log("WARN 未知模型名 %q：不在目录内，原样透传给上游（可能被网关兜底或报错）", name)
	}
	return name
}

// firstSystemSnippet 取第一条 system 消息的文本片段（临时诊断用）。
// content 可能是字符串，也可能是多段数组（视觉消息）。
func firstSystemSnippet(msgs []any) (string, bool) {
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "system" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return c, true
		case []any:
			var b strings.Builder
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if s, ok := pm["text"].(string); ok {
						b.WriteString(s)
					}
				}
			}
			if b.Len() > 0 {
				return b.String(), true
			}
		}
	}
	return "", false
}

// qoderClientSessionKey 客户端显式会话标识：header 优先，其次 body。
func qoderClientSessionKey(r *http.Request, raw map[string]any) string {
	if v := strings.TrimSpace(r.Header.Get("X-Session-Id")); v != "" {
		return v
	}
	if v, ok := raw["conversation_id"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// sessionLease 取/建会话租约；访问时顺带清理过期项（不起 goroutine）。
func (p *QoderProxy) sessionLease(key, uid string) *qoderSessionLease {
	now := time.Now()
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.sessionCache == nil {
		p.sessionCache = make(map[string]*qoderSessionLease)
	}
	for k, l := range p.sessionCache {
		if now.Sub(l.lastSeen) > qoderSessionLeaseTTL {
			delete(p.sessionCache, k)
		}
	}
	if l, ok := p.sessionCache[key]; ok {
		l.lastSeen = now
		cp := *l
		return &cp
	}
	l := &qoderSessionLease{
		SessionID:    qoderSessionID(uid),
		RequestSetID: "qoder-request-" + qoderUUID(),
		BusinessID:   qoderUUID(),
		lastSeen:     now,
	}
	p.sessionCache[key] = l
	cp := *l
	return &cp
}

// qoderMaxConcurrent 当前生效的上游并发上限；0 = 不限制（默认）。
//
// 每请求现读，改 QODER_MAX_CONCURRENT 或数据根 qoder_max_concurrent.txt
// 之后不用重启。非法值/负数一律回落默认（0），超封顶按封顶算。
func qoderMaxConcurrent() int {
	if n, ok := qoderIntSetting(qoderMaxConcurrentEnv, qoderMaxConcurrentFile); ok {
		if n < 0 {
			return qoderMaxConcurrentDefault
		}
		if n > qoderMaxConcurrentCeil {
			return qoderMaxConcurrentCeil
		}
		return n
	}
	return qoderMaxConcurrentDefault
}

// qoderMinGap 当前生效的相邻请求最小间隔；0 = 不等待。
// 同样每请求现读，QODER_MIN_GAP_MS / qoder_min_gap.txt 可改。
func qoderMinGap() time.Duration {
	n := qoderMinGapDefault
	if v, ok := qoderIntSetting(qoderMinGapEnv, qoderMinGapFile); ok {
		n = v
	}
	if n < 0 {
		n = qoderMinGapDefault
	}
	if n > qoderMinGapCeil {
		n = qoderMinGapCeil
	}
	return time.Duration(n) * time.Millisecond
}

// acquireUpstreamSlot 抢并发槽。上限 <= 0 时完全放行（默认）；池满返回 false
// （调用方回本地 429）。release 幂等，重复调用不会把计数减穿。
func (p *QoderProxy) acquireUpstreamSlot() (release func(), ok bool) {
	limit := qoderMaxConcurrent()
	p.inflightMu.Lock()
	if limit > 0 && p.inflight >= limit {
		p.inflightMu.Unlock()
		return nil, false
	}
	p.inflight++
	p.inflightMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.inflightMu.Lock()
			if p.inflight > 0 {
				p.inflight--
			}
			p.inflightMu.Unlock()
		})
	}, true
}

// inflightCount 在途请求数（日志/测试用）。
func (p *QoderProxy) inflightCount() int {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	return p.inflight
}

// waitHumanGap 相邻上游请求的最小间隔。持有 lastReqMu 等待，保证并发请求
// 也按间隔排队；ctx 取消（客户端断开）时立即放弃。
// 间隔配成 0（QODER_MIN_GAP_MS=0）时直接放行，不抢锁、不等待。
func (p *QoderProxy) waitHumanGap(ctx context.Context) bool {
	gap := qoderMinGap()
	if gap <= 0 {
		return true
	}
	p.lastReqMu.Lock()
	defer p.lastReqMu.Unlock()
	gap += time.Duration(rand.Int63n(int64(qoderMinGapJitter))) * time.Millisecond
	if wait := time.Until(p.lastReq.Add(gap)); wait > 0 {
		if !sleepCtx(ctx, wait) {
			return false
		}
	}
	p.lastReq = time.Now()
	return true
}

// qoderIntSetting 读整数型配置：环境变量优先，其次数据根文件，都没有返回 ok=false。
func qoderIntSetting(env, file string) (int, bool) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	b, err := os.ReadFile(dataPath(file))
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (p *QoderProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if p.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "代理未就绪", "type": "auth_error"},
		})
		return
	}

	var raw map[string]any
	rawBytes, rerr := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if rerr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "read body error", "type": "invalid_request_error"},
		})
		return
	}
	dec := json.NewDecoder(bytes.NewReader(rawBytes))
	dec.UseNumber()
	if dec.Decode(&raw) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad json", "type": "invalid_request_error"},
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

	// 模型名可能缺失或不是字符串：显式处理，避免 fmt 把 nil 变成字面量 "<nil>"
	// （那会是一个非法模型名，白白触发上游兜底）。
	requestedModel := ""
	if mv, ok := raw["model"].(string); ok {
		requestedModel = mv
	}

	// 模型解析 + 思考意图解析 → qodercli 信封（裸 OpenAI 体走网关兜底，
	// 思考链路整体失效，2026-09-25 根因定论，见 qoder_envelope.go）。
	modelKey := p.resolveModel(requestedModel)
	normMsgs := qoderNormalizeMessages(msgs)
	// 上游按「自报家门的身份句 / 框架标签」拦请求（dom 线路 11128 同款根因），
	// qoder 线路此前完全没接清洗。QODER_SANITIZE=0 可关（排查用）。
	if qoderSanitizeEnabled() {
		normMsgs = sanitizeHarnessMessages(normMsgs)
	}
	markedNoThink := qoderNoThinkMarked(normMsgs)
	if markedNoThink {
		qoderStripNoThinkMarker(normMsgs)
	}
	entry := p.entryFor(modelKey)
	// 消息内标记 = 本次明确不要思考（最具体，优先级最高）
	spec := qoderThinkingSpec{Effort: "none"}
	if !markedNoThink {
		spec = qoderResolveThinking(raw, normMsgs, entry)
	}
	uid, _, _, _ := p.auth.Snapshot()
	lease := p.sessionLease(qoderLeaseKey(qoderClientSessionKey(r, raw), normMsgs), uid)
	env := qoderEnvelope(normMsgs, raw["tools"], modelKey, uid, spec, entry, raw, lease)
	// 可见思考提示兜底：默认关闭，QODER_THINK_PROMPT=on 显式开启
	if qoderThinkPromptEnabled() && spec.Effort != "none" {
		qoderInjectPrompt(env, qoderThinkingPrompt)
	}
	// 网关流式工具 bug 自动降级：DeepSeek/Kimi 系 stream=true + 工具历史 → 上游改非流式
	// （详见 qoder_stream.go 降级注释；QODER_AUTO_NOSTREAM=0 关闭）。
	degrade := false
	if clientStream && qoderAutoNoStreamEnabled() && qoderStreamToolBugModels[modelKey] && qoderHistoryHasToolMsgs(normMsgs) {
		degrade = true
		env["stream"] = false
	}
	payload, _ := json.Marshal(env)

	rid := randHex(4)
	modelName := modelKey
	if p.log != nil {
		p.log.Log("[%s] ▶ REQUEST %s | stream=%v | msgs=%d | payload=%dKB%s", rid, modelName, clientStream, len(msgs),
			len(payload)/1024, lastUserSuffix(msgs))
		p.log.Log("[%s] ▶ THINK %s", rid, qoderWireDiag(raw, normMsgs, entry, markedNoThink, env))
		// 诊断：记录客户端请求形态。触发方式 = 环境变量 QODER_DIAG_STREAM=1
		// 或在数据目录放标记文件 qoder-dump-on（存在即抓，含完整请求转储）。
		diagOn := os.Getenv("QODER_DIAG_STREAM") == "1" || fileExists(dataPath("qoder-dump-on"))
		if diagOn {
			nTools := 0
			if t, ok := raw["tools"].([]any); ok {
				nTools = len(t)
			}
			names := make([]string, 0, 4)
			if t, ok := raw["tools"].([]any); ok {
				for i, tv := range t {
					if i >= 4 {
						break
					}
					if tm, ok := tv.(map[string]any); ok {
						if fn, ok := tm["function"].(map[string]any); ok {
							names = append(names, fmt.Sprintf("%v", fn["name"]))
						}
					}
				}
			}
			p.log.Log("[%s] ▶ DIAG client | tools=%d(%v) tool_choice=%v stream=%v topEffort=%v thinking=%v enable_thinking=%v | payload=%dKB msgs=%d",
				rid, nTools, strings.Join(names, ","), raw["tool_choice"], raw["stream"],
				raw["reasoning_effort"], raw["thinking"], raw["enable_thinking"], len(payload)/1024, len(msgs))
			if sysMsg, ok := firstSystemSnippet(normMsgs); ok {
				// 只留长度：明文 system 不再落盘（converter-qoder.log 曾积 95 条明文提示词）
				p.log.Log("[%s] ▶ DIAG system提示词长度=%d字", rid, len(sysMsg))
				qid := currentQoderIdentity()
				p.log.Log("[%s] ▶ DIAG outbound | ide=%s clientType=%s session_type=%s product=%s | system=%d字 harness=%d acp=%d | session=%s",
					rid, qid.Version, qid.ClientType, qid.SessionType, qid.Product,
					len(sysMsg), strings.Count(sysMsg, "DeepSeek Harness"), strings.Count(sysMsg, "<acp"), lease.SessionID)
			}
			// 完整客户端请求 dump（原始 OpenAI 形态，可直接重放）
			if fb, err := json.MarshalIndent(raw, "", " "); err == nil {
				dumpPath := dataPath("qoder-dump-" + rid + ".json")
				os.WriteFile(dumpPath, fb, 0600)
				p.log.Log("[%s] ▶ DIAG 已 dump 完整请求: %s (%d 字节)", rid, dumpPath, len(fb))
				qoderPruneDumps(p.log) // DIAG dump 也会积（1MB/份），同样只留最近几份
			}
		}
	}
	t0 := time.Now()

	release, ok := p.acquireUpstreamSlot()
	if !ok {
		limit := qoderMaxConcurrent()
		if p.log != nil {
			p.log.Log("[%s] ⚠ 本地并发上限 %d，拒绝请求（在途 %d）", rid, limit, p.inflightCount())
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("本地并发上限 %d，请稍后重试", limit),
				"type":    "rate_limit_error",
			},
		})
		return
	}
	defer release()
	if !p.waitHumanGap(r.Context()) {
		return // 客户端已断开
	}

	if clientStream {
		if degrade {
			if p.log != nil {
				p.log.Log("[%s] ⚠ 网关流式工具bug命中(%s|stream=true|工具历史)：按 QODER_AUTO_NOSTREAM=1 预判式降级为非流式聚合后回灌SSE", rid, modelKey)
			}
			p.degradedStreamToClient(w, r, payload, modelName, t0, rid)
			return
		}
		// 2026-10-19：默认走真流式。上游若真的复现当年的 provider_error，
		// streamToClient 会在「写 SSE 头之前」发现并返回 retryDegraded=true，
		// 这里再降级重试一次——既能享受真流式，又保留兜底。
		if p.streamToClient(w, r, payload, t0, rid) {
			if p.log != nil {
				p.log.Log("[%s] ⚠ 真流式失败，回退为「非流式聚合后回灌SSE」（%s|工具历史）", rid, modelKey)
			}
			p.degradedStreamToClient(w, r, payload, modelName, t0, rid)
		}
		return
	}
	p.aggregateToClient(w, r, payload, modelName, t0, rid)
}
