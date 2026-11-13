package main

// 果办（OfficeAce）模型线路：vault STS → 网关三要素（appKey/appSecret/base）
// → Basic 认证透传 chat。接入 lineSpecs/lineAdapters，与 mimo/oc 同级。
//
// 实测链路（2026-11-13）：
//   POST https://agentarts.cn-southwest-2.myhuaweicloud.com/v1/claw/client-subscription
//        （SDK-HMAC 签名，body={}，头 x-subscription-type: v2）
//   → { model_info: { model_auth_info: { model_app_key, model_app_secret },
//                     model_api_url_base: "modelgw-0006.officeace.…-agentarts.com" } }
//   POST https://{base}/v2/chat/completions（Basic appKey:appSecret）
//   → 200 chat.completion（OpenAI 兼容，含 reasoning_content 思考段）
//
// 三要素缓存：TTL 30 分钟或「401 时立即失效」。凭据过期（STS 1.5h）由
// huawei_vault.go 的 hwFreshSTS 在每次「取三要素」时检查/续期；Basic 认证
// 的 appKey/appSecret 是网关侧的长效凭据，不受 STS 生命周期影响（缓存住
// 就不用频繁打订阅端点）。
//
// 模型表：先静态（插件同款 7 个，全 1M ctx），目录探测 {base}/v2/models
// 成功后动态替换——保持「无订阅也能起线路」的降级能力。

import (
	"encoding/base64"
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
	HuaweiPort = 8802 // 果办线路本地端口（挨着 oc 的 8800，避开已占用的 8787/8790/8799）

	hwClawBase = "https://agentarts.cn-southwest-2.myhuaweicloud.com"

	// hwGatewayTTL 三要素缓存时长。订阅侧给的 model_info 理论上开通期内稳定；
	// 401（STS 过期后签名被拒）会立刻作废缓存走续期重取。
	hwGatewayTTL = 30 * time.Minute
)

// hwGateway 网关三要素缓存（进程级，线路重启不清）。
type hwGateway struct {
	mu      sync.Mutex
	appKey  string
	appSec  string
	base    string
	fetched time.Time
}

var hwGw hwGateway

// hwGatewayInfo 取网关三要素（缓存优先）。
func hwGatewayInfo() (appKey, appSecret, base string, err error) {
	hwGw.mu.Lock()
	defer hwGw.mu.Unlock()
	if hwGw.appKey != "" && time.Since(hwGw.fetched) < hwGatewayTTL {
		return hwGw.appKey, hwGw.appSec, hwGw.base, nil
	}
	cred, _, ferr := hwFreshSTS()
	if ferr != nil {
		return "", "", "", ferr
	}
	body, status, nerr := httpPostJSON(hwClawBase+"/v1/claw/client-subscription",
		signHuaweiSDK(cred, "POST", hwClawBase+"/v1/claw/client-subscription", []byte("{}"), map[string]string{
			"content-type":        "application/json;charset=utf8",
			"x-subscription-type": "v2",
		}), []byte("{}"))
	if nerr != nil {
		return "", "", "", fmt.Errorf("网关信息获取失败（网络错误）: %w", nerr)
	}
	if status != http.StatusOK {
		return "", "", "", fmt.Errorf("网关信息获取失败（HTTP %d）：%s", status, truncate(string(body), 160))
	}
	var j struct {
		ModelInfo struct {
			ModelAuthInfo struct {
				ModelAppKey    string `json:"model_app_key"`
				ModelAppSecret string `json:"model_app_secret"`
			} `json:"model_auth_info"`
			ModelAPIURLBase string `json:"model_api_url_base"`
		} `json:"model_info"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return "", "", "", fmt.Errorf("网关信息响应解析失败: %w", err)
	}
	if j.ModelInfo.ModelAuthInfo.ModelAppKey == "" || j.ModelInfo.ModelAuthInfo.ModelAppSecret == "" {
		return "", "", "", fmt.Errorf("网关信息里没有 model_app_key/app_secret（可能未开通订阅）")
	}
	hwGw.appKey = j.ModelInfo.ModelAuthInfo.ModelAppKey
	hwGw.appSec = j.ModelInfo.ModelAuthInfo.ModelAppSecret
	// 上游下发的是裸主机名（实测 "modelgw-0006.officeace.…-agentarts.com"，
	// 不带 scheme）——这里归一化成完整 URL，否则 http.NewRequest 会报
	// "unsupported protocol scheme"。Node 探针里做过同样处理，Go 侧漏了。
	base = strings.TrimSpace(j.ModelInfo.ModelAPIURLBase)
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	hwGw.base = strings.TrimRight(base, "/")
	hwGw.fetched = time.Now()
	return hwGw.appKey, hwGw.appSec, hwGw.base, nil
}

// hwGatewayInvalidate 401 时作废缓存（下次重新取）。
func hwGatewayInvalidate() {
	hwGw.mu.Lock()
	hwGw.appKey = ""
	hwGw.fetched = time.Time{}
	hwGw.mu.Unlock()
}

// hwBasicAuth Basic 认证头。
func hwBasicAuth(appKey, appSecret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(appKey+":"+appSecret))
}

// HuaweiProxy 果办线路代理。不复用 Proxy 结构（那是「JSON 凭证/Bearer」的
// 形状，Basic + 双签名的果办差太多），自建小 HTTP 服务，端口语义一致。
type HuaweiProxy struct {
	mu     sync.Mutex
	server *http.Server
	ln     net.Listener
	models []string
}

// hwClientVersion 果办线路对外自称的版本（仅 UA 标识，不参与鉴权）。
const hwClientVersion = "0.4.5"

// hwStaticModels 静态模型表（插件 FALLBACK_MODELS 同款；目录探测成功后动态刷新）。
var hwStaticModels = []string{
	"deepseek-v4.1-flash", // 倍率 0.18，默认模型
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"glm-5.3", // 0.79
	"glm-5.2",
	"glm-5.1",
	"kimi-k3", // 2.38（目录里带换行，这里转义掉）
}

// NewHuaweiProxy 果办线路。
func NewHuaweiProxy() *HuaweiProxy {
	return &HuaweiProxy{models: append([]string{}, hwStaticModels...)}
}

// Running 线路是否在监听。
func (p *HuaweiProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.server != nil
}

// Start 绑定端口开始服务（照 mimo 的端口清扫姿势）。
func (p *HuaweiProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	// 先验凭据：vault 没账号就别起（报「先在 DSH 登录」）
	if _, _, err := hwFreshSTS(); err != nil {
		return false, "果办凭据不可用：" + err.Error()
	}
	ln, err := listenLinePort(HuaweiPort)
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", HuaweiPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", p.handleHealth)
	mux.HandleFunc("/v1/models", p.handleModels)
	mux.HandleFunc("/v1/chat/completions", p.handleChat)
	p.mu.Lock()
	p.server = &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	p.ln = ln
	p.mu.Unlock()
	go func() {
		defer recoverGoroutine("huawei_proxy.go:serve")
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			logHub("果办线路 serve error: %v", err)
		}
	}()
	logHub("果办线路已启动 127.0.0.1:%d", HuaweiPort)
	return true, "果办线路已启动"
}

// Stop 停线路。
func (p *HuaweiProxy) Stop() bool {
	p.mu.Lock()
	srv := p.server
	p.server = nil
	p.ln = nil
	p.mu.Unlock()
	if srv == nil {
		return true
	}
	return srv.Close() == nil
}

func (p *HuaweiProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "line": "huawei"})
}

func (p *HuaweiProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	// 探测动态目录（失败回静态表——「没起订阅也能列模型」）
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": p.modelList()})
}

func (p *HuaweiProxy) modelList() []any {
	p.mu.Lock()
	models := append([]string{}, p.models...)
	p.mu.Unlock()
	out := make([]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{"id": m, "object": "model", "owned_by": "huawei-officeace"})
	}
	return out
}

// modelNames 模型名列表（状态面板显示用）。
func (p *HuaweiProxy) modelNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.models...)
}

// handleChat 转发 /v1/chat/completions → {base}/v2/chat/completions。
func (p *HuaweiProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "read body: " + err.Error()}})
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "bad json: " + err.Error()}})
		return
	}
	model, _ := raw["model"].(string)
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "model is required"}})
		return
	}
	model = hwMapModel(model)

	// 网关三要素（缓存优先；401 时作废重取一次）
	appKey, appSecret, base, gerr := hwGatewayInfo()
	if gerr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "果办网关不可用：" + gerr.Error(), "type": "auth_error"},
		})
		return
	}

	payload, _ := json.Marshal(raw)
	url := strings.TrimRight(base, "/") + "/v2/chat/completions"

	rid := randHex(4)
	logHub("果办[%s] ▶ %s | stream=%v | bytes=%d", rid, model, raw["stream"] == true, len(body))

	// 一次 401 重试：STS 签的 model_info 缓存可能横跨 STS 过期
	resp, err := hwDoUpstream(url, appKey, appSecret, payload)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		hwGatewayInvalidate()
		appKey, appSecret, base, gerr = hwGatewayInfo()
		if gerr == nil {
			url = strings.TrimRight(base, "/") + "/v2/chat/completions"
			resp, err = hwDoUpstream(url, appKey, appSecret, payload)
		}
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "upstream: " + err.Error()}})
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		if strings.HasPrefix(k, "X-") || k == "Content-Type" {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, 256<<20))
	logHub("果办[%s] ◀ HTTP %d | %s", rid, resp.StatusCode, model)
}

// hwMapModel 别名归一（目录 id 带尾巴的都映射回短名）。
func hwMapModel(name string) string {
	n := strings.TrimSpace(strings.ReplaceAll(name, "\n", ""))
	switch {
	case strings.HasPrefix(n, "deepseek-v4.1-flash"):
		return "deepseek-v4.1-flash"
	case strings.HasPrefix(n, "deepseek-v4-flash"):
		return "deepseek-v4-flash"
	case strings.HasPrefix(n, "glm-5.3-flash"):
		return "glm-5.3-flash"
	case strings.HasPrefix(n, "glm-5.3"):
		return "glm-5.3"
	case strings.HasPrefix(n, "glm-5.2"):
		return "glm-5.2"
	case strings.HasPrefix(n, "glm-5.1"):
		return "glm-5.1"
	case strings.HasPrefix(n, "kimi-k3"):
		return "kimi-k3"
	}
	return n
}

// hwDoUpstream 发上游 chat 请求（Basic 认证，流式原样透传由调用方处理）。
func hwDoUpstream(url, appKey, appSecret string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", hwBasicAuth(appKey, appSecret))
	// 身份表（line_identity.go）登记的就是这个 UA，测试会核对一致性
	req.Header.Set("User-Agent", "OfficeAce/"+hwClientVersion)
	cli := backendClient(120 * time.Second) // chat 可能长思考
	return cli.Do(req)
}
