package main

// QoderProxy 服务：启动/停止/HTTP 路由/模型解析
// 由 proxy_qoder.go 拆分而来（纯文件搬运，未改逻辑）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"os"
)

// ---------- 代理服务 ----------

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

	mu     sync.Mutex
	ln     net.Listener
	server *http.Server
}

func NewQoderProxy() *QoderProxy {
	catalog := defaultQoderCatalog()
	return &QoderProxy{
		Port:     QoderPort,
		inferURL: QoderInferURL,
		catalog:  catalog,
		aliases:  buildQoderAliases(catalog),
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
	p.client = directClient()

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
		stopPort(p.Port) // 清掉残留（如独立版 qoder_proxy.exe）
	}
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", p.Port, err)
	}

	srv := &http.Server{Handler: p, ReadHeaderTimeout: 20 * time.Second}
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

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !portListening(p.Port) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !portListening(p.Port)
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
		if id != m.Key {
			entry["resolves_to"] = m.Key // 透出真实 key，便于排查
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

	body := map[string]any{}
	for k, v := range raw {
		body[k] = v
	}
	// 模型名可能缺失或不是字符串：显式处理，避免 fmt 把 nil 变成字面量 "<nil>"
	// （那会是一个非法模型名，白白触发上游兜底）。
	requestedModel := ""
	if mv, ok := raw["model"].(string); ok {
		requestedModel = mv
	}
	body["model"] = p.resolveModel(requestedModel)
	body["messages"] = qoderNormalizeMessages(msgs)
	body["stream"] = true // 上游一律流式，非流式由本地聚合
	markedNoThink := qoderNoThinkMarked(body)
	entry := p.entryFor(fmt.Sprintf("%v", body["model"]))
	qoderApplyThinking(body, entry)
	payload, _ := json.Marshal(body)

	rid := randHex(4)
	modelName := fmt.Sprintf("%v", body["model"])
	if p.log != nil {
		p.log.Log("[%s] ▶ REQUEST %s | stream=%v | msgs=%d%s", rid, modelName, clientStream, len(msgs),
			lastUserSuffix(msgs))
		p.log.Log("[%s] ▶ THINK %s", rid, qoderThinkDiag(raw, body, entry, markedNoThink))
		// 临时诊断：记录客户端请求形态（tools / 思考字段），用于核对 DSH 实际发了什么
		if os.Getenv("QODER_DIAG_STREAM") == "1" {
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
			p.log.Log("[%s] ▶ DIAG client | tools=%d(%v) tool_choice=%v stream=%v topEffort=%v thinking=%v enable_thinking=%v",
				rid, nTools, strings.Join(names, ","), raw["tool_choice"], raw["stream"],
				raw["reasoning_effort"], raw["thinking"], raw["enable_thinking"])
			if sysMsg, ok := firstSystemSnippet(msgs); ok {
				p.log.Log("[%s] ▶ DIAG system提示词(前300字) | %s", rid, truncate(sysMsg, 300))
				p.log.Log("[%s] ▶ DIAG system提示词长度=%d字", rid, len(sysMsg))
			}
			// 完整请求体 dump（仅诊断模式，写独立文件，便于精确复现）
			if os.Getenv("QODER_DIAG_DUMP") == "1" {
				if fb, err := json.MarshalIndent(raw, "", " "); err == nil {
					dumpPath := dataPath("qoder-dump-" + rid + ".json")
					os.WriteFile(dumpPath, fb, 0644)
					p.log.Log("[%s] ▶ DIAG 已 dump 完整请求: %s (%d 字节)", rid, dumpPath, len(fb))
				}
			}
		}
	}
	t0 := time.Now()

	if clientStream {
		p.streamToClient(w, r, payload, t0, rid)
		return
	}
	p.aggregateToClient(w, r, payload, modelName, t0, rid)
}


