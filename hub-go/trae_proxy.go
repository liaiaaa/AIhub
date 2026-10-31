package main

// Trae CN（字节）反代：桌面端凭据 → 本地 OpenAI 兼容路由（127.0.0.1:8795）。
//
// 协议逆向自多个开源实现互证（2026-10 调研，照抄核对过源码）：
//   - connectedGraph/trae2api-web internal/upstream/{constants,headers,payload,solosse}.go
//   - laojichao/trae-local-api（tc 解密 + 四版本支持）
//   - muskke/trae-api-proxy
//
// 要点：
//   - 聊天端点 POST {traeAgentHost}/api/agent/v3/llm_utils_chat，
//     function 固定 "solo_work_lite"（其他值实测无效）。
//   - 认证 Authorization: Cloud-IDE-JWT <jwt>（不是 Bearer！），重复放
//     X-Cloudide-Token / X-Ide-Token；无每请求签名（不像 Qoder COSY）。
//   - 上游按 X-Ide-Version 门控模型：0.1.43 请求 glm-5.3 报 4001，
//     0.1.52（versionCode 20260811）正常，这里固定用 0.1.52。
//   - 上游 SSE 是自定义事件（metadata/output/token_usage/done/error），
//     这里转写成标准 OpenAI chunk 流；非流式本地聚合。
//   - 只可反代 SOLO 免费通道（消耗 ide_credits）；work 通道需要
//     encrypted_prompt_set（只在本地 agent 进程的加密 DB 里），不可反代。
//   - token 过期（401）先重读桌面端 storage.json（桌面端会自动刷新），
//     无效再用 refreshToken 走 ExchangeToken（只更新内存，不回写桌面端文件）。

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	TraePort = 8795

	traeAgentHost  = "https://trae-api-cn.mchost.guru"
	traeUgHost     = "https://api.trae.cn"
	traeOAuthHost  = "https://api.trae.com.cn"
	traeAppID      = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeIdeVersion = "0.1.52"
	traeVerCode    = "20260811"
	traeFunction   = "solo_work_lite"

	traeEpChat     = "/api/agent/v3/llm_utils_chat"
	traeEpExchange = "/cloudide/api/v3/trae/oauth/ExchangeToken"
)

// TraeDefaultModel 默认模型（glm-5.2 实测可用）。
const TraeDefaultModel = "glm-5.2"

// traeIdleEnv 上游连续多久没有任何字节算「卡死」（秒），默认 120。
//
// 2026-10-27 用户报 trae kimi-k3 一次对话在 dsh 里等一分钟没消息。实测该模型
// 先吐一大段 reasoning_content，正文要等思考结束才出来；一旦上游静默，客户端
// 又没有超时，就会一直干等。这里加空闲看门狗：超过阈值主动中止并回一条明确的
// 错误帧，客户端至少能看到「为什么失败」。
// 思考模型首帧十几秒是常态，所以默认给到 120s，可用环境变量调小。
const (
	traeIdleEnv     = "AIHUB_TRAE_IDLE_SECS"
	traeIdleDefault = 120 * time.Second
)

// traeEffortEnv 客户端没带 reasoning_effort 时给 trae 注入的默认思考档位。
//
// 2026-10-27 实测（同一份 127KB 载荷、100 个工具、各两次）：
//
//	默认：14.4s / 20.2s；reasoning_effort=low：9.1s / 4.4s。
//
// kimi-k3 的思考帧从 1079 降到 65，几乎全花在 thinking 上，所以低档位明显更快。
// 默认留空 = 不注入，保持上游默认；要提速就设 AIHUB_TRAE_EFFORT=low。
const traeEffortEnv = "AIHUB_TRAE_EFFORT"

// traeEffortFile 放在数据根下的档位文件（与 zcode_api_key.txt 同一套约定）：
// 桌面快捷方式启动时环境变量不好带，写文件更省事；每个请求现读，改完不用重启。
const traeEffortFile = "trae_effort.txt"

// traeEffortLevels 允许的档位（上游实测接受 low；medium/high 保留给以后试）。
var traeEffortLevels = map[string]bool{"low": true, "medium": true, "high": true}

func traeDefaultEffort() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(traeEffortEnv)))
	if v == "" && hubRoot != "" {
		if b, err := os.ReadFile(filepath.Join(hubRoot, traeEffortFile)); err == nil {
			v = strings.ToLower(strings.TrimSpace(string(b)))
		}
	}
	if !traeEffortLevels[v] {
		return ""
	}
	return v
}

// traeApplyDefaultEffort 在客户端没显式给档位时补一个默认值，返回实际注入的值。
func traeApplyDefaultEffort(raw map[string]any) string {
	eff := traeDefaultEffort()
	if eff == "" {
		return ""
	}
	if _, ok := raw["reasoning_effort"]; ok {
		return ""
	}
	raw["reasoning_effort"] = eff
	return eff
}

func traeIdleTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv(traeIdleEnv))
	if v == "" {
		return traeIdleDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return traeIdleDefault
	}
	return time.Duration(n) * time.Second
}

// TraeProxy 本地反代实例。零值不可用，须 NewTraeProxy。
type TraeProxy struct {
	Port int
	log  *FileLogger

	mu    sync.Mutex
	auth  *traeAuthInfo
	dev   traeDeviceInfo
	ln    net.Listener
	cli   *http.Client
	portS *http.Server
}

// TraeModels 国内版可用模型（0.1.52 实测 config_name；动态目录后续可加）。
var TraeModels = []string{
	"glm-5.2", "glm-5", "glm-5-turbo", "glm-5.3",
	"qwen-3.7-plus",
	"Doubao-Seed-2.0-Code", "Doubao-Seed-2.1-Turbo", "Doubao-Seed-2.1-Pro",
	"DeepSeek-V4-Pro", "DeepSeek-V4-Flash", "deepseek-v4.1-flash",
	"kimi-k2.6", "kimi-k2.7-code", "kimi-k3",
	"minimax-m3",
	"custom_model_gpt-5", "custom_model_gemini", "custom_model_deepseek_reasoner",
}

func NewTraeProxy() *TraeProxy {
	return &TraeProxy{Port: TraePort}
}

func (p *TraeProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

func (p *TraeProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	info, dev, err := traeReadAuth()
	if err != nil {
		return false, err.Error()
	}
	ln, err := listenLinePort(p.Port)
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", p.Port, err)
	}
	tr := directClient().Transport
	p.mu.Lock()
	p.auth, p.dev = info, dev
	p.cli = &http.Client{Transport: tr}
	p.ln = ln
	p.portS = newLineServer(p)
	srv := p.portS
	p.mu.Unlock()

	if p.log == nil {
		p.log = NewFileLogger(dataPath("converter-trae.log"))
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.Log("serve error: %v", err)
		}
	}()
	p.log.Log("==== proxy 启动 ====")
	return true, "已启动"
}

func (p *TraeProxy) Stop() bool {
	p.mu.Lock()
	srv, ln := p.portS, p.ln
	p.portS, p.ln = nil, nil
	p.mu.Unlock()
	closeLineServer(srv, ln)
	return waitPortFree(p.Port, lineShutdownGrace)
}

func (p *TraeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (p *TraeProxy) handleHealth(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	a := p.auth
	p.mu.Unlock()
	info := map[string]any{"status": "ok", "backend": traeAgentHost}
	if a != nil {
		info["uid"] = a.UserID
		info["token_expired_at"] = a.ExpiredAt
	}
	writeJSON(w, http.StatusOK, info)
}

func (p *TraeProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]any, 0, len(TraeModels))
	for _, m := range TraeModels {
		data = append(data, map[string]any{"id": m, "object": "model", "created": 1700000000, "owned_by": "trae-cn"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// snapshot 取当前登录态（只读）。
func (p *TraeProxy) snapshot() (*traeAuthInfo, traeDeviceInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auth, p.dev
}

// setAuth 更新内存登录态（401 自愈用，不回写桌面端文件）。
func (p *TraeProxy) setAuth(info *traeAuthInfo, dev traeDeviceInfo) {
	p.mu.Lock()
	p.auth, p.dev = info, dev
	p.mu.Unlock()
}

// traeSOLOHeaders 聊天/模型接口请求头（照抄 trae2api-web SOLOHeaders）。
func traeSOLOHeaders(req *http.Request, a *traeAuthInfo, dev traeDeviceInfo) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "Trae/"+traeIdeVersion)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.Token)
	req.Header.Set("X-Cloudide-Token", a.Token)
	req.Header.Set("X-Ide-Token", a.Token)
	if a.UserID != "" {
		req.Header.Set("X-Uid", a.UserID)
	}
	req.Header.Set("X-App-Id", traeAppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", traeIdeVersion)
	req.Header.Set("X-Ide-Version-Code", traeVerCode)
	req.Header.Set("X-App-Version-Code", traeVerCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", "Windows 11 Pro")
	req.Header.Set("X-Device-Brand", "83DG")
	req.Header.Set("Request-Traffic-Type", "prod")
	if dev.MachineID != "" {
		req.Header.Set("X-Machine-Id", dev.MachineID)
	}
	if dev.DeviceID != "" {
		req.Header.Set("X-Device-Id", dev.DeviceID)
	}
}

// traePrepareBody OpenAI → SOLO llm_utils_chat 请求体改写（照抄 trae2api-web payload.go）：
//  1. content 字符串 → [{"type":"text","text":...}]（数组透传）
//  2. assistant.tool_calls 的 function → function_call（上游字段名），name 空的剔除
//  3. tools[].function.parameters 对象 → JSON 字符串（上游 Go struct 是 string）
//  4. tool_choice 归一化：none→删 tools；auto/required→字符串；function→取 name
//  5. model → config_name + model；stream 强制 true
func traePrepareBody(src map[string]any, model string) map[string]any {
	obj := cloneMap(src)
	obj["stream"] = true
	obj["function"] = traeFunction

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			// 2026-10-20：dsh（OpenAI 新规范）会发 role=developer，Trae 上游只认
			// ['system','assistant','user','tool','function']，见了 developer 整包
			// 拒绝并回 code=4027「developer is not one of ...」。qoder 侧同款问题
			// 在 qoderNormalizeMessages 里映射 developer→system，这里保持一致。
			// （用户 16:52 报 Trae 在聚合口和 Codex 里都调不通，根因就是它。）
			if role == "developer" {
				m["role"] = "system"
			}
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}
			content, present := m["content"]
			if !present || content == nil {
				continue
			}
			if c, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": c}}
			}
		}
	}

	model = strings.TrimSpace(model)
	if model == "" {
		model = TraeDefaultModel
	}
	obj["config_name"] = model
	obj["model"] = model

	// tool_choice 归一化
	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				delete(obj, "tools")
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "none":
				delete(obj, "tool_choice")
				delete(obj, "tools")
			case "auto", "required":
				obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if strings.TrimSpace(name) != "" {
					obj["tool_choice"] = strings.TrimSpace(name)
				} else {
					obj["tool_choice"] = "auto"
				}
			default:
				delete(obj, "tool_choice")
			}
		default:
			delete(obj, "tool_choice")
		}
	}

	// tools[].function.parameters → JSON 字符串
	if list, ok := obj["tools"].([]any); ok {
		out := make([]any, 0, len(list))
		for _, item := range list {
			t, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := t["function"].(map[string]any)
			if !ok {
				continue
			}
			if params, ok := fn["parameters"].(map[string]any); ok {
				if s, err := json.Marshal(params); err == nil {
					fn["parameters"] = string(s)
				}
			}
			out = append(out, t)
		}
		if len(out) == 0 {
			delete(obj, "tools")
		} else {
			obj["tools"] = out
		}
	}
	return obj
}

func (p *TraeProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	authReady := p.auth != nil
	p.mu.Unlock()
	if !authReady {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "代理未就绪", "type": "auth_error"},
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
	requested, _ := raw["model"].(string)

	injectedEffort := traeApplyDefaultEffort(raw)
	body := traePrepareBody(raw, requested)
	payload, _ := json.Marshal(body)
	rid := randHex(4)
	modelName, _ := body["model"].(string)
	if p.log != nil {
		effNote := ""
		if injectedEffort != "" {
			effNote = " | effort=" + injectedEffort
		}
		p.log.Log("[%s] ▶ REQUEST %s | stream=%v | msgs=%d | payload=%dKB%s%s", rid, modelName, clientStream, len(msgs), len(payload)/1024, effNote, lastUserSuffix(msgs))
	}

	// 请求 ctx 一路传到上游：客户端断开（dsh 里 Esc / 关页面）时立刻中止上游，
	// 2026-10-27 实测过一次「dsh 71s 就 abort、hub 还把上游跑到 141s」的浪费。
	// 看门狗也会用这个 cancel 主动中止卡死的上游。
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, err := p.chatUpstream(ctx, payload, rid)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		if ctx.Err() != nil {
			// 客户端已断开导致的取消，不必回错误体（也写不出去）
			return
		}
		traeWriteError(w, clientStream, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		msg := traeUpstreamErrMsg(resp.StatusCode, string(b))
		traeWriteError(w, clientStream, resp.StatusCode, msg)
		return
	}

	t0 := time.Now()
	if clientStream {
		p.streamToClient(ctx, cancel, w, resp.Body, modelName, rid, t0)
		return
	}
	p.aggregateToClient(ctx, w, resp.Body, modelName, rid, t0)
}

// chatUpstream 发起对话请求；401 时重读桌面端凭据再试一次（桌面端会自动刷新
// JWT），仍失败则尝试 refreshToken 兑换。
func (p *TraeProxy) chatUpstream(ctx context.Context, payload []byte, rid string) (*http.Response, error) {
	info, dev := p.snapshot()
	resp, err := p.doChat(ctx, info, dev, payload)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if p.log != nil {
		p.log.Log("[%s] ⚠ 401，重读桌面端凭据自愈", rid)
	}
	if fresh, dev2, err := traeReadAuth(); err == nil && fresh.Token != info.Token {
		p.setAuth(fresh, dev2)
		if resp, err = p.doChat(ctx, fresh, dev2, payload); err == nil && resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
		}
	}
	// 重读没拿到新 token：用 refreshToken 兑换（双 ClientID 逐个试，只更新内存）
	if refreshed := p.exchangeToken(info); refreshed != nil {
		p.setAuth(refreshed, dev)
		if resp, err = p.doChat(ctx, refreshed, dev, payload); err == nil {
			return resp, nil
		}
		if err == nil {
			resp.Body.Close()
		}
		return nil, fmt.Errorf("token 已失效且兑换失败，请在 Trae 桌面端重新登录")
	}
	return nil, fmt.Errorf("token 已失效（401），请打开 Trae 桌面端刷新登录")
}

func (p *TraeProxy) doChat(ctx context.Context, info *traeAuthInfo, dev traeDeviceInfo, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", traeAgentHost+traeEpChat, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	traeSOLOHeaders(req, info, dev)
	p.mu.Lock()
	cli := p.cli
	p.mu.Unlock()
	return cli.Do(req)
}

// exchangeToken refreshToken → 新 JWT（响应为轮换的新 refreshToken，仅存内存）。
func (p *TraeProxy) exchangeToken(old *traeAuthInfo) *traeAuthInfo {
	for _, clientID := range []string{"ono9krqynydwx5", "en1oxy7wnw8j9n"} { // CN IDE / SOLO stable
		body, _ := json.Marshal(map[string]any{
			"ClientID": clientID, "RefreshToken": old.RefreshToken, "ClientSecret": "-", "UserID": old.UserID,
		})
		req, err := http.NewRequest("POST", traeOAuthHost+traeEpExchange, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Trae/"+traeIdeVersion)
		p.mu.Lock()
		cli := p.cli
		p.mu.Unlock()
		resp, err := cli.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var out struct {
			Code int             `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(b, &out) != nil || out.Code != 0 || len(out.Data) == 0 {
			if p.log != nil {
				p.log.Log("ExchangeToken(%s) 失败: HTTP %d %s", clientID, resp.StatusCode, truncate(string(b), 160))
			}
			continue
		}
		var na traeAuthInfo
		if json.Unmarshal(out.Data, &na) == nil && na.Token != "" {
			if na.RefreshToken == "" {
				na.RefreshToken = old.RefreshToken
			}
			na.UserID = old.UserID
			if p.log != nil {
				p.log.Log("ExchangeToken(%s) 成功，token 已内存续期", clientID)
			}
			return &na
		}
	}
	return nil
}

func traeUpstreamErrMsg(status int, body string) string {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) == nil {
		if msg, ok := m["message"].(string); ok && msg != "" {
			return fmt.Sprintf("HTTP %d: %s", status, msg)
		}
	}
	return fmt.Sprintf("HTTP %d: %s", status, truncate(body, 300))
}

// traeWriteError 流式（SSE error 事件 + [DONE]）与非流式（JSON）统一的错误出口。
func traeWriteError(w http.ResponseWriter, stream bool, code int, msg string) {
	if !stream {
		writeJSON(w, code, map[string]any{
			"error": map[string]any{"message": msg, "type": "upstream_error", "code": code},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": code}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
}

// ---------- SOLO SSE → OpenAI SSE（照抄 trae2api-web solosse.go 的转换逻辑） ----------

// traeSSEEvent 归一化的上游事件。
type traeSSEEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	FinishReason string
	ErrCode      int64
	ErrMsg       string
}

// traeScanLine 解析一行；事件边界（空行）时返回解析结果。
func traeScanLine(st *traeSSEState, line string) *traeSSEEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.data.Reset()
			return nil
		}
		ev := &traeSSEEvent{Event: st.event, ErrMsg: st.data.String()}
		raw := st.data.String()
		st.event = ""
		st.data.Reset()
		if raw == "" {
			return ev
		}
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) != nil {
			return ev
		}
		switch ev.Event {
		case "output":
			ev.Response, _ = m["response"].(string)
			ev.Reasoning, _ = m["reasoning_content"].(string)
			if tc, ok := m["tool_calls"]; ok && tc != nil {
				ev.ToolCalls, _ = json.Marshal(tc)
			}
		case "token_usage":
			ev.Usage = m
		case "done":
			ev.FinishReason, _ = m["finish_reason"].(string)
		case "error":
			if v, ok := m["code"].(float64); ok {
				ev.ErrCode = int64(v)
			}
			ev.ErrMsg, _ = m["message"].(string)
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		// SSE 规范：一个事件的多行 data 之间用 \n 连接（客户端会把它们
		// 拼成一个以 \n 分隔的整体）。2026-10-21 开源审查修：原来直接
		// WriteString 无分隔拼接，上游若发多行 JSON 会拼出非法 JSON 被静默丢帧。
		if st.data.Len() > 0 {
			st.data.WriteByte('\n')
		}
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	}
	return nil
}

type traeSSEState struct {
	event string
	data  bytes.Buffer
}

// traeCleanToolCall SOLO 工具调用字段 → OpenAI 标准（function_call→function，
// 去掉 namespace/partial_arguments）。
func traeCleanToolCall(call map[string]any) {
	if fc, ok := call["function_call"].(map[string]any); ok {
		call["function"] = fc
		delete(call, "function_call")
	}
	if fn, ok := call["function"].(map[string]any); ok {
		delete(fn, "namespace")
		delete(fn, "partial_arguments")
	}
}

func (p *TraeProxy) streamToClient(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, body io.Reader, model, rid string, t0 time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := fmt.Sprintf("chatcmpl-%s", randHex(8))
	var pendingUsage map[string]any
	thinkFrames := 0
	contentBytes := 0
	firstFrameAt := -1.0 // 首帧耗时：区分「hub 慢」还是「上游慢」的唯一实测指标
	sawDone := false
	writeChunk := func(delta map[string]any, finish string) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model": model, "choices": []any{choice},
		}
		if pendingUsage != nil {
			chunk["usage"] = pendingUsage
			pendingUsage = nil
		}
		b, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	roleSent := false
	ensureRole := func() error {
		if !roleSent {
			roleSent = true
			return writeChunk(map[string]any{"role": "assistant", "content": ""}, "")
		}
		return nil
	}
	// clientGone：写失败说明客户端走了。必须停下来——继续读上游每一帧都在烧 trae 额度。
	clientGone := func() {
		if p.log != nil {
			p.log.Log("[%s] ⚠ 客户端已断开，中止上游读取 | 首帧=%.1fs", rid, firstFrameAt)
		}
	}

	// 空闲看门狗：连续 traeIdleTimeout() 没有任何帧 → cancel 请求 ctx，让传输层
	// 掐断上游 body。2026-10-27 用户报「等一分钟没消息」；有真卡死时至少要给出错误，
	// 而不是让客户端无限等。
	lastAt := atomic.Int64{}
	lastAt.Store(time.Now().UnixNano())
	timedOut := atomic.Bool{}
	done := make(chan struct{})
	defer close(done)
	go func() {
		idle := traeIdleTimeout()
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tk.C:
				if time.Since(time.Unix(0, lastAt.Load())) >= idle {
					timedOut.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	st := &traeSSEState{}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		lastAt.Store(time.Now().UnixNano())
		ev := traeScanLine(st, strings.TrimRight(sc.Text(), "\r\n"))
		if ev == nil {
			continue
		}
		if firstFrameAt < 0 {
			firstFrameAt = time.Since(t0).Seconds()
		}
		switch ev.Event {
		case "output":
			delta := map[string]any{}
			if ev.Response != "" {
				delta["content"] = ev.Response
				contentBytes += len(ev.Response)
			}
			if ev.Reasoning != "" {
				delta["reasoning_content"] = ev.Reasoning
				thinkFrames++
			}
			if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
				var tc []map[string]any
				if json.Unmarshal(ev.ToolCalls, &tc) == nil {
					for _, call := range tc {
						traeCleanToolCall(call)
					}
					delta["tool_calls"] = tc
				}
			}
			if len(delta) > 0 {
				if err := ensureRole(); err != nil {
					clientGone()
					return
				}
				if err := writeChunk(delta, ""); err != nil {
					clientGone()
					return
				}
			}
		case "token_usage":
			pendingUsage = ev.Usage
			// 2026-10-21 开源审查修：token_usage 原先只是暂存，靠「下一个」
			// chunk 搭出去。Trae 的发送顺序不固定 —— 若 usage 在 done 之后才到，
			// 后面已经没有 chunk 了，这条 usage 就永远丢掉（用量统计少记）。
			// 已经发过 finish 时，单独补一个 usage-only chunk。
			if sawDone {
				if err := writeChunk(map[string]any{}, ""); err != nil {
					clientGone()
					return
				}
			}
		case "done":
			if err := writeChunk(map[string]any{}, ev.FinishReason); err != nil { // writeChunk 会带上暂存的 usage
				clientGone()
				return
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			sawDone = true
		case "error":
			if sawDone {
				// [DONE] 之后的帧客户端不会再读，写出去只是浪费（还会让
				// 严格的客户端报「DONE 之后仍有事件」）。
				continue
			}
			msg := fmt.Sprintf("Trae 上游错误 code=%d: %s", ev.ErrCode, ev.ErrMsg)
			if p.log != nil {
				p.log.Log("[%s] ✗ 上游错误帧 | %s", rid, truncate(msg, 300))
			}
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": ev.ErrCode}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
			sawDone = true
		}
	}
	// 收尾优先级：上游空闲超时 > 客户端断开/取消 > 正常断流兜底。
	if timedOut.Load() {
		msg := fmt.Sprintf("Trae 上游 %d 秒没有任何数据，hub 已中止本次请求（不是客户端问题）", int(traeIdleTimeout().Seconds()))
		if p.log != nil {
			p.log.Log("[%s] ✗ %s | 首帧=%.1fs", rid, msg, firstFrameAt)
		}
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_timeout"}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	if err := ctx.Err(); err != nil {
		// 客户端断开 / 上层取消：对端已经不读了，别再算兜底 [DONE]
		if p.log != nil {
			p.log.Log("[%s] ⚠ 请求已取消（客户端断开），上游读取中止 | 首帧=%.1fs", rid, firstFrameAt)
		}
		return
	}
	if !sawDone {
		// 上游断流兜底：保证一定有 [DONE]
		if err := writeChunk(map[string]any{}, "stop"); err != nil {
			clientGone()
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | stream | 首帧=%.1fs | think=%d帧 | content=%d字节",
			rid, time.Since(t0).Seconds(), firstFrameAt, thinkFrames, contentBytes)
	}
}

func (p *TraeProxy) aggregateToClient(ctx context.Context, w http.ResponseWriter, body io.Reader, model, rid string, t0 time.Time) {
	var content, reasoning strings.Builder
	toolCalls := map[int]map[string]any{}
	var toolOrder []int
	var usage map[string]any
	finish := "stop"
	var upErr string
	var upErrCode int64

	st := &traeSSEState{}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		ev := traeScanLine(st, strings.TrimRight(sc.Text(), "\r\n"))
		if ev == nil {
			continue
		}
		switch ev.Event {
		case "output":
			content.WriteString(ev.Response)
			reasoning.WriteString(ev.Reasoning)
			traeMergeToolCalls(toolCalls, &toolOrder, ev.ToolCalls)
		case "token_usage":
			usage = ev.Usage
		case "done":
			if ev.FinishReason != "" {
				finish = ev.FinishReason
			}
		case "error":
			upErrCode = ev.ErrCode
			upErr = fmt.Sprintf("code=%d: %s", ev.ErrCode, ev.ErrMsg)
		}
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | finish=%s | think=%d段", rid, time.Since(t0).Seconds(), finish, len(reasoning.String()))
	}
	if ctx.Err() != nil {
		// 客户端已断开：结果没人要了，也不用回错误体（写不出去）
		return
	}
	if upErr != "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "Trae 上游错误 " + upErr, "type": "upstream_error", "code": upErrCode},
		})
		return
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id": "chatcmpl-" + randHex(8), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	writeJSON(w, http.StatusOK, resp)
}

// traeMergeToolCalls 按 index 合并流式工具调用片段（arguments 拼接）。
func traeMergeToolCalls(merged map[int]map[string]any, order *[]int, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]any{one}
	}
	for _, call := range arr {
		if call == nil {
			continue
		}
		idx := 0
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		m, seen := merged[idx]
		if !seen {
			m = map[string]any{"index": idx}
			merged[idx] = m
			*order = append(*order, idx)
		}
		if v, ok := call["id"].(string); ok && v != "" {
			m["id"] = v
		}
		if v, ok := call["type"].(string); ok && v != "" {
			m["type"] = v
		}
		df, _ := call["function"].(map[string]any)
		if df == nil {
			df, _ = call["function_call"].(map[string]any)
		}
		if df == nil {
			continue
		}
		mf, _ := m["function"].(map[string]any)
		if mf == nil {
			mf = map[string]any{}
			m["function"] = mf
		}
		if v, ok := df["name"].(string); ok && v != "" {
			mf["name"] = v
		}
		if v, ok := df["arguments"].(string); ok && v != "" {
			if prev, _ := mf["arguments"].(string); prev != "" {
				mf["arguments"] = prev + v
			} else {
				mf["arguments"] = v
			}
		}
	}
}
