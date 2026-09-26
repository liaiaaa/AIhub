package main

// ZcodeProxy 服务：Z.ai（Zcode）API 反向代理，本地 OpenAI 兼容路由。
//
// 支持两种端点（控制台/环境变量可切换）：
//   1. Coding Plan 端点：https://api.z.ai/api/coding/paas/v4（Coding Plan 配额）
//   2. General API 端点：https://api.z.ai/api/paas/v4（余额/预付费包）
//
// 与 Qoder/workbuddy 代理的能力对齐（2026-09-26）：
//   - 模型目录：内置快照（Z.ai 无公开 /models 接口），显示名双轨别名，
//     /v1/models 透出视觉/上下文/输出上限/思考档位元数据
//   - 思考控制：reasoning_effort / thinking.type / enable_thinking / [no-thinking]
//     → Z.ai 的 thinking.type + reasoning_effort（GLM-5.3 系不可关闭，压到 low）
//   - 流处理：原生 delta.reasoning_content 透传 + <think>/<thinking> 标签分流，
//     残留帧在 [DONE] 前吐出；上游错误帧原样转发（Z.ai 本就是 OpenAI error 形态）
//   - 自愈重试：网络错误 / 429 / 5xx 最多 3 次尝试（700ms 间隔）
//   - 凭据：控制台传入 > 环境变量 ZCODE_API_KEY > 数据目录 zcode_api_key.txt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// ZcodePort 是 Zcode 代理的默认端口
	ZcodePort = 8796

	// ZcodeDefaultEndpoint 默认使用 Coding Plan 端点（GLM Coding Plan 配额）
	ZcodeDefaultEndpoint = "https://api.z.ai/api/coding/paas/v4"

	// ZcodeGeneralEndpoint 通用 API 端点（使用余额）
	ZcodeGeneralEndpoint = "https://api.z.ai/api/paas/v4"

	// zcodeDefaultModel 默认模型（必须是上游真实 id，不是显示名）
	zcodeDefaultModel = "glm-5.3"
)

// ZcodeProxy 一个本地代理实例。零值不可用，须用 NewZcodeProxy 初始化。
type ZcodeProxy struct {
	Port         int
	Endpoint     string // Z.ai API 端点
	APIKey       string // Z.ai API Key（启动前设置；为空时启动时自动回退加载）
	DefaultModel string

	// 模型目录与别名表（zcode_models.go 提供，内置快照）
	catalogMu sync.RWMutex
	catalog   []zcodeModelEntry
	aliases   map[string]string

	mu     sync.Mutex
	client *http.Client
	server *http.Server
	ln     net.Listener

	log    *FileLogger
	auth   *ZcodeAuth // 桌面端凭据（自动解密）；nil = 使用手动 API Key
	apiKey string     // Start 时落定的运行时密钥
}

// NewZcodeProxy 创建一个新的 Zcode 代理实例。
func NewZcodeProxy() *ZcodeProxy {
	catalog := zcodeDefaultCatalog()
	return &ZcodeProxy{
		Port:         ZcodePort,
		Endpoint:     ZcodeDefaultEndpoint,
		DefaultModel: zcodeDefaultModel,
		catalog:      catalog,
		aliases:      buildZcodeAliases(catalog),
	}
}

// catalogSnapshot 返回当前目录副本。
func (p *ZcodeProxy) catalogSnapshot() []zcodeModelEntry {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	out := make([]zcodeModelEntry, len(p.catalog))
	copy(out, p.catalog)
	return out
}

// entryFor 按真实 key 查目录条目（未收录返回 nil）。
func (p *ZcodeProxy) entryFor(key string) *zcodeModelEntry {
	return zcodeEntryFor(p.catalogSnapshot(), key)
}

// aliasLookup 查别名表。
func (p *ZcodeProxy) aliasLookup(name string) (string, bool) {
	p.catalogMu.RLock()
	defer p.catalogMu.RUnlock()
	if p.aliases == nil {
		return "", false
	}
	key, ok := p.aliases[qoderNormalizeAlias(name)]
	return key, ok
}

// resolveModel 把客户端传来的模型名归一为上游真实 id。
// 未命中 → 原样透传（Z.ai 上新模型无需改代理），并记 WARN 便于发现拼写错误。
func (p *ZcodeProxy) resolveModel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return p.DefaultModel
	}
	if key, ok := p.aliasLookup(name); ok {
		return key
	}
	if p.log != nil {
		p.log.Log("WARN 未知模型名 %q：不在目录内，原样透传给上游", name)
	}
	return name
}

// Running 检查代理是否正在运行。
func (p *ZcodeProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

// Start 绑定端口并开始服务。
func (p *ZcodeProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	if p.DefaultModel == "" {
		p.DefaultModel = zcodeDefaultModel
	}
	if p.Endpoint == "" {
		p.Endpoint = ZcodeDefaultEndpoint
	}

	// 凭据链：控制台手动 Key 优先；否则自动解密桌面端凭据
	// （%USERPROFILE%\.zcode\v2\credentials.json，与 Qoder 桌面端同款零配置体验）；
	// 都没有再回退到控制台保存过的 zcode_api_key.txt
	if p.APIKey == "" {
		if a, aerr := NewZcodeAuth(); aerr == nil {
			p.auth = a
			p.APIKey = a.Key()
		} else if k, ferr := LoadAPIKeyFromFile(); ferr == nil {
			p.APIKey = k
		} else {
			return false, "未找到 Zcode 桌面端凭据：" + aerr.Error()
		}
	} else if a, aerr := NewZcodeAuth(); aerr == nil {
		p.auth = a // 手动 Key 仍优先生效，auth 仅用于 401 自愈与账号展示
	}
	p.apiKey = p.APIKey

	p.log = NewFileLogger(dataPath("converter-zcode.log"))
	// api.z.ai 国内外均可直连，不读系统/环境代理（与 Qoder/dom 一致）
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
		stopPort(p.Port)
	}
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", p.Port, err)
	}

	srv := &http.Server{Handler: p, ReadHeaderTimeout: 20 * time.Second}
	p.mu.Lock()
	p.server = srv
	p.ln = ln
	p.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			if p.log != nil {
				p.log.Log("serve error: %v", err)
			}
		}
	}()
	if p.log != nil {
		p.log.Log("==== zcode proxy 启动 | endpoint=%s | models=%d ====", p.Endpoint, len(p.catalogSnapshot()))
	}
	logHub("zcode proxy started on 127.0.0.1:%d (endpoint: %s)", p.Port, p.Endpoint)
	return true, "已启动（Zcode · " + p.Endpoint + "）"
}

// Stop 关闭本实例的监听并释放端口。
func (p *ZcodeProxy) Stop() bool {
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

// ServeHTTP 处理 HTTP 请求。
func (p *ZcodeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

// handleHealth 健康检查接口。
func (p *ZcodeProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"status":     "ok",
		"backend":    p.Endpoint,
		"proxy_port": p.Port,
		"model":      p.DefaultModel,
		"models":     len(p.catalogSnapshot()),
	}
	if p.auth != nil {
		info["credential"] = p.auth.Summary()
	} else if p.apiKey != "" {
		head := p.apiKey
		if len(head) > 8 {
			head = head[:8] + "..."
		}
		info["credential"] = map[string]any{"type": "zai_api_key", "key_head": head}
	}
	writeJSON(w, http.StatusOK, info)
}

// handleModels 模型清单：id 用官方显示名（双轨别名，请求时映射回真实 key），
// 并透出能力元数据（视觉/上下文/输出上限/思考档位），供客户端渲染选项。
func (p *ZcodeProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	catalog := p.catalogSnapshot()
	data := make([]map[string]any, 0, len(catalog))
	seen := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		id := m.DisplayName
		if id == "" {
			id = m.Key
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		entry := map[string]any{
			"id": id, "object": "model", "created": 1700000000, "owned_by": m.Vendor,
		}
		if id != m.Key {
			entry["resolves_to"] = m.Key
		}
		entry["vision"] = m.IsVision
		if m.MaxInputTokens > 0 {
			entry["max_input_tokens"] = m.MaxInputTokens
		}
		if m.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = m.MaxOutputTokens
		}
		if len(m.ContextWindows) > 0 {
			entry["context_windows"] = m.ContextWindows
		}
		if m.IsReasoning {
			thinking := map[string]any{"supported": true, "can_disable": m.SupportsDisable}
			if len(m.Efforts) > 0 {
				thinking["efforts"] = m.Efforts
				thinking["default_effort"] = m.DefaultEffort
			}
			entry["thinking"] = thinking
		}
		data = append(data, entry)
	}
	defKey := zcodeDefaultKey(catalog)
	defID := defKey
	for _, m := range catalog {
		if m.Key == defKey && m.DisplayName != "" {
			defID = m.DisplayName
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

func (p *ZcodeProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]any{"message": "method not allowed", "type": "invalid_request_error"},
		})
		return
	}
	if p.apiKey == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "代理未就绪（缺少 API Key）", "type": "auth_error"},
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
	dec := json.NewDecoder(strings.NewReader(string(rawBytes)))
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

	// 模型解析 + 思考意图 → Z.ai wire 字段
	requestedModel := ""
	if mv, ok := raw["model"].(string); ok {
		requestedModel = mv
	}
	modelKey := p.resolveModel(requestedModel)
	entry := p.entryFor(modelKey)

	body := map[string]any{}
	for _, k := range passthroughKeys {
		if v, ok := raw[k]; ok {
			body[k] = v
		}
	}
	normMsgs := qoderNormalizeMessages(msgs)
	markedNoThink := qoderNoThinkMarked(normMsgs)
	if markedNoThink {
		qoderStripNoThinkMarker(normMsgs)
	}
	body["model"] = modelKey
	body["messages"] = normMsgs
	body["stream"] = true // 上游一律流式，非流式由本地聚合
	zcodeApplyThinking(body, entry, markedNoThink)
	// 输出上限按模型钳制（5.3 系 128K）
	if entry != nil && entry.MaxOutputTokens > 0 {
		if n, ok := qoderIntVal(body["max_tokens"]); ok && n > entry.MaxOutputTokens {
			body["max_tokens"] = entry.MaxOutputTokens
		}
	}
	payload, _ := json.Marshal(body)

	rid := randHex(4)
	if p.log != nil {
		p.log.Log("[%s] ▶ REQUEST %s | stream=%v | msgs=%d%s", rid, modelKey, clientStream, len(msgs),
			lastUserSuffix(msgs))
		p.log.Log("[%s] ▶ THINK %s", rid, zcodeThinkDiag(raw, markedNoThink, entry, body))
	}
	t0 := time.Now()

	if clientStream {
		p.streamToClient(w, r, payload, t0, rid)
		return
	}
	p.aggregateToClient(w, r, payload, modelKey, t0, rid)
}
