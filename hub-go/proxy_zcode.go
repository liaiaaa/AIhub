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
	"os/exec"
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

	// zcodeRelayProbeURL 本地 start-plan 中继（tools\zcode-relay\，zcode-api
	// 引擎）：免费额度通道。Start 时自动探测，探测到即切换；ZCODE_RELAY_URL
	// =off 可强制关闭，ZCODE_RELAY_URL 指定其他地址；本地密钥解析见
	// zcode_relay.go 的 zcodeRelayKey（env > relay-key 文件 > 历史常量）。
	zcodeRelayProbeURL  = "http://127.0.0.1:8807"
	zcodeRelayProbeTTL  = 5 * time.Minute
	zcodeRelayProbeWait = 800 * time.Millisecond
)

// ZcodeProxy 一个本地代理实例。零值不可用，须用 NewZcodeProxy 初始化。
type ZcodeProxy struct {
	Port         int
	Endpoint     string // Z.ai API 端点
	APIKey       string // Z.ai API Key（启动前设置；为空时启动时自动回退加载）
	DefaultModel string

	// 模型目录与别名表（zcode_models.go 提供，内置快照；
	// zcode_catalog_refresh.go 以 client/configs 实时目录刷新）
	catalogMu     sync.RWMutex
	catalog       []zcodeModelEntry
	aliases       map[string]string
	catalogLiveAt time.Time // 最近一次实时目录成功时间
	catalogTryAt  time.Time // 最近一次尝试时间（失败冷却用）

	// 身份头 / 端点路由 / 套餐额度（2026-09-27 zcode-api 逆向接入）
	identity *zcodeIdentity
	routing  *zcodeRouting
	stopCh   chan struct{}

	// 本地 start-plan 中继（免费额度）：非空 = 中继模式
	relayURL        string
	relayKey        string
	relayProbeAt    time.Time // 上次探测时间（探测失败后冷却重试）
	relayProbeMutex sync.Mutex

	// 本实例拉起的中继栈进程（点「停止」时一并收掉；外部手动启动的不动）
	spawnedMintCmd  *exec.Cmd
	spawnedRelayCmd *exec.Cmd
	relayReadyOnce  *sync.Once

	// 验证码熔断器：IP 风控惩罚期内 token 连续被拒，继续重铸只会喂深惩罚。
	// 连续 N 轮 3007 自救失败 → 熔断打开，请求秒回明确错误；退避到期后
	// 放行真实流量试探，成功即复位，仍被拒则加倍退避（封顶 30min）。
	captchaFails        int
	captchaBreakerUntil time.Time

	// 中继请求启动节流（防验证码风控爆发特征，见 zcode_relay.go）
	relayStartMu   sync.Mutex
	relayNextStart time.Time

	quotaMu        sync.Mutex
	quotaBuckets   []zcodeQuotaBucket
	quotaFetchedAt time.Time
	quotaError     string

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

	// 桌面端身份（User-Agent/X-Device-Mid 等，请求指纹对齐官方客户端）
	p.identity = zcodeLoadIdentity()
	// 服务端端点改写表（agent/configs，fail-open）
	p.routing = newZcodeRouting(p.identity, func() string { return p.apiKey })
	// 中继栈自拉起 + 探测接入：正常 8~12s（mint 2s + 引擎 2~5s + 探测）。
	// 有界等待让控制台「启动」返回时状态就是对的；超时则交后台循环兜底。
	relayReady := make(chan struct{})
	p.relayReadyOnce = new(sync.Once)
	go func() {
		defer p.relayReadyOnce.Do(func() { close(relayReady) })
		p.spawnRelayStack()
		p.detectRelay(true)
	}()
	select {
	case <-relayReady:
	case <-time.After(12 * time.Second):
	}
	// 后台刷新：实时模型目录 + 额度 + 中继探测（Stop 收敛）
	p.stopCh = make(chan struct{})
	go p.backgroundLoop()

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

// backgroundLoop 周期刷新实时目录与额度视图。所有刷新各自带节流与
// fail-open 语义：失败只影响新鲜度，绝不动已服务的目录/路由。
func (p *ZcodeProxy) backgroundLoop() {
	stopCh := p.stopCh
	if n, err := p.refreshCatalog(); err != nil {
		p.logf("实时模型目录首刷失败（保留内置快照）：%v", err)
	} else {
		p.logf("实时模型目录已刷新：%d 个模型", n)
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-t.C:
			if !p.Running() {
				continue
			}
			// 中继未启用时周期重探测 + 兜底自拉起（中继崩溃后自动复活）
			p.detectRelay(false)
			if _, _, on := p.relayTarget(); !on {
				p.spawnRelayStack()
				p.detectRelay(true)
			}
			if n, err := p.refreshCatalog(); err != nil && p.log != nil {
				p.log.Log("实时模型目录刷新失败（保留现有目录）：%v", err)
			} else if p.log != nil {
				p.log.Log("实时模型目录刷新：%d 个模型", n)
			}
		}
	}
}

// logf 日志便捷封装（log 未初始化时静默）。
func (p *ZcodeProxy) logf(format string, args ...any) {
	if p.log != nil {
		p.log.Log(format, args...)
	}
}

// Stop 关闭本实例的监听并释放端口。
func (p *ZcodeProxy) Stop() bool {
	p.mu.Lock()
	srv, ln, stopCh := p.server, p.ln, p.stopCh
	p.server, p.ln = nil, nil
	p.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
		p.stopCh = nil
	}

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
	// 点「停止」：本实例拉起的中继栈（mint/中继）一并收掉
	p.stopSpawnedRelayStack()

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
		"models":     len(zcodeFilterCatalog(p.catalogSnapshot())),
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

	// 实时目录新鲜度
	p.catalogMu.RLock()
	catalogLive := !p.catalogLiveAt.IsZero()
	catalogLiveAt := p.catalogLiveAt
	p.catalogMu.RUnlock()
	info["catalog_source"] = map[string]any{
		"mode":         orBoolStr(catalogLive, "live(client/configs)", "builtin_snapshot"),
		"refreshed_at": catalogLiveAt.Format("2006-01-02 15:04:05"),
	}

	// 身份（脱敏：设备号只露头）
	if p.identity != nil {
		mid := p.identity.deviceMid
		if len(mid) > 8 {
			mid = mid[:8] + "..."
		}
		info["identity"] = map[string]any{
			"app_version": p.identity.appVersion,
			"platform":    p.identity.platformParam(),
			"os_version":  p.identity.osVersion,
			"timezone":    p.identity.clientTimezone,
			"device_mid":  mid,
		}
	}
	// 端点路由快照
	info["endpoint_routing"] = map[string]any{
		"snapshot": p.routing.HasSnapshot(),
		"entries":  p.routing.SnapshotSize(),
	}
	// 本地 start-plan 中继（免费额度通道）
	if url, _, ok := p.relayTarget(); ok {
		relayInfo := map[string]any{
			"mode": "start_plan_free_quota",
			"url":  url,
		}
		if wait, open := p.captchaBreakerOpen(); open {
			relayInfo["captcha_breaker"] = map[string]any{
				"open":             true,
				"retry_after_secs": int(wait.Seconds()) + 1,
			}
		} else {
			relayInfo["captcha_breaker"] = map[string]any{"open": false}
		}
		info["relay"] = relayInfo
	} else {
		info["relay"] = map[string]any{
			"mode": "direct_api_key",
			"hint": "免费额度通道未启用：运行 tools\\zcode-relay\\start-zcode-relay.bat 后 5 分钟内自动接入（或重启本代理立即生效）",
		}
	}
	// 套餐额度（只读 billing/balance，5min 缓存）
	if buckets, qerr := p.quotaSnapshot(); len(buckets) > 0 {
		info["quota"] = buckets
	} else if qerr != "" {
		info["quota"] = map[string]any{"error": qerr}
	}
	writeJSON(w, http.StatusOK, info)
}

func orBoolStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// handleModels 模型清单：id 用官方显示名（双轨别名，请求时映射回真实 key），
// 并透出能力元数据（视觉/上下文/输出上限/思考档位），供客户端渲染选项。
// 只透出白名单内的模型（当前套餐额度覆盖的两个），内部目录仍保留全量。
func (p *ZcodeProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	catalog := zcodeFilterCatalog(p.catalogSnapshot())
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
