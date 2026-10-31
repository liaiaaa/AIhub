package main

// OpenCode 免费线路（2026-10-15）。
//
// 上游：https://opencode.ai/inference/v1/chat/completions —— 标准 OpenAI 兼容，
// 支持流式与函数调用。opencode 的免费档走「匿名 public 凭据」，不是用户自己的
// OAuth token（拿 OAuth 打这个端点会撞 FreeTierError）。
//
// 2026-10-14 起上游加了免费档闸门，逐项实测确认的通过条件如下。任一项不满足都是
// 403 FreeTierError，而报错文案只说「只能在 OpenCode 内使用」、不指出缺哪一项，
// 所以这里把每条都记下来：
//
//  1. 必须 stream:true —— 非流式直接 403。客户端要非流式时，我们收流再聚合。
//  2. 必须带官方那 5 个核心 agent 工具（shell/edit/glob/grep/read），而且
//     description 与 parameters 要和官方 CLI **逐字节一致**（合成占位 stub 会 403）。
//     真定义内嵌在 assets/opencode_tools.json。工具顺序无关，客户端自己的工具可以
//     额外带上（所以 DSH 的 function calling 照常工作）。
//  3. 会话头四个必须同时存在且同值：
//     x-opencode-session / x-opencode-session-id / x-session-affinity / X-Session-Id
//  4. 会话 id 必须是官方 Identifier.descending() 的布局：
//     "ses_" + 12 位降序毫秒时间戳 hex + 14 位 Base62。
//     随机 hex 即使形状对也 403（低 12 位要落在 counter 1-63 即 0xFC0-0xFFF 窗口）。
//  5. x-opencode-project 必须是**裸 40 位小写 hex**；官方 prj_* 规范 id 反而 403。
//  6. **不能发 x-opencode-request**（2026-10-14 起该头被拒）。
//  7. UA 形状 opencode/latest/<ver>/cli；Authorization 必须是字面量 "Bearer public"
//     （大小写敏感，"Bearer PUBLIC" 401；缺头也 401）。
//  8. 区域：opencode.ai 对部分地区返回 RegionError，需海外出口。这里复用
//     newBackendClient()（读环境变量/系统代理），与 intl 线路同一套出网逻辑。
//
// 只挂免费模型（用户明确要求「我只要免费的，剩下的不要」），且每个都带上下文窗口
// 与输出上限（用户要求「模型一定要标记清楚上下文为多少」）。
//
// 逆向依据：jasonxu114514/opencode2api（Go，★651，2026-10-15 仍在更新）的
// internal/gateway/upstream.go 与 internal/identity/request.go。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpencodePort 本线路本地端口（8800：hub-go 线路段里唯一未占用）。
const OpencodePort = 8800

const (
	opencodeBaseURL = "https://opencode.ai/inference"
	opencodeAnonKey = "public"
	// opencodeClientVer 上游按 UA 里的版本做兼容判定，与本机 CLI 对齐。
	opencodeClientVer = "2.0.29"
)

// opencodeCoreTools 免费档要求的官方核心工具集（缺 shell 或 read 直接 403）。
var opencodeCoreTools = []string{"shell", "edit", "glob", "grep", "read"}

// opencodeModel 一个免费模型的静态元数据。
type opencodeModel struct {
	ID      string   // 上游模型 id
	Name    string   // 显示名
	Context int      // 上下文窗口（入）
	Output  int      // 最大输出
	Vision  bool     // 是否支持图片输入
	Efforts []string // 支持的思考档位
	NoChat  bool     // 上游不收 chat/completions（只能走 /v1/responses）
	NoResp  bool     // 上游不收 /v1/responses（只能走 chat/completions）
}

// opencodeFreeModels 免费模型清单。上下文/输出/模态/档位取自 opencode 官方目录
// （本机 `GET :49374/api/model` 的 limit 与 capabilities 字段）。
// 「匿名档可见」逐个实测：nemotron-3-ultra-free 官方目录有、匿名档没有，故不列。
//
// NoChat / NoResp 是 2026-10-15 **逐个实测**出来的上游协议支持面，结果正好互补：
// 只有 muse-spark 收 responses，其余 8 个只收 chat（打错协议上游回
// ModelProtocolUnsupported）。所以两张表都要填准，让线路在本地就给出
// 可读的「请走另一条路径」，而不是把上游那句看不懂的原话透出去。
// opencodeFreeModels 挂进线路的免费模型。
//
// 2026-10-18 按用户规则收窄：**上下文窗口不足 1M 的一律不挂**（"少于1M上下文的就不要了"），
// 加上当时实测已死和太慢的，9 个里只剩 2 个。逐条依据：
//
//	longcat-2.5-preview-free   1M     ✓ 实测 3.2s 正常
//	space-bunny-free           1M     ✓ 实测 1.2s 正常
//	cat-jam-free               256K   按 <1M 规则摘除（当时可用）
//	mimo-v2.6-flash-free       200K   按 <1M 规则摘除（且 23s 才出结果）
//	big-pickle                 200K   按 <1M 规则摘除
//	nemotron-3.5-lightning-free 256K  用户明确"不要他了"（且首字要 51s，DSH 默认超时会杀）
//	step-5-preview-free        1M     上游稳定 HTTP 500 无响应体（连打 3 次）
//	deepseek-v4.1-flash-free   200K   上游稳定 HTTP 500 无响应体；它还是 dsh agent-default-model
//	                              指向的模型，用户那次会话就是被它卡住的
//	muse-spark-1.3-contributor-free 1M 上游已下架（目录里只剩 muse-spark-1.3），chat 400 / responses 500
//
// 想补新免费模型前先确认两件事：上游 /inference/v1/models 里有它 **且** 实测能出正文
// （目录不等于匿名档真能跑：cat-jam-free 能跑却不在目录里），以及拿得到上下文窗口数值
// —— 标不出上下文的模型不挂，这是用户定的规矩。
var opencodeFreeModels = []opencodeModel{
	{ID: "longcat-2.5-preview-free", Name: "LongCat 2.5 Preview Free", Context: 1000000, Output: 131072, Vision: true, NoResp: true},
	{ID: "space-bunny-free", Name: "Space Bunny Free", Context: 1048576, Output: 524288, Vision: true, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, NoResp: true},
}

// opencodeModelIDs id → 元数据（校验请求模型名用）。
var opencodeModelIDs = func() map[string]opencodeModel {
	m := make(map[string]opencodeModel, len(opencodeFreeModels))
	for _, x := range opencodeFreeModels {
		m[x.ID] = x
	}
	return m
}()

// ---------- 官方工具定义 ----------

type opencodeToolDef struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

var (
	opencodeToolsOnce sync.Once
	opencodeToolsMap  map[string]opencodeToolDef
)

// opencodeTools 读内嵌的官方工具定义。上游逐字节比对，所以不能用合成的占位描述。
func opencodeTools() map[string]opencodeToolDef {
	opencodeToolsOnce.Do(func() {
		opencodeToolsMap = map[string]opencodeToolDef{}
		b, err := assets.ReadFile("assets/opencode_tools.json")
		if err != nil {
			logHub("opencode 工具定义资产缺失：%v（免费档请求会被 403 拒）", err)
			return
		}
		if err := json.Unmarshal(b, &opencodeToolsMap); err != nil {
			logHub("opencode 工具定义解析失败：%v", err)
			opencodeToolsMap = map[string]opencodeToolDef{}
		}
	})
	return opencodeToolsMap
}

// opencodeCoreToolWire 把一个核心工具渲染成 chat 形态定义；
// 官方定义缺失时退回最小形状（会被上游拒，但不至于 panic）。
func opencodeCoreToolWire(name string) map[string]any {
	desc := "Agent tool " + name
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	if d, ok := opencodeTools()[name]; ok {
		desc = d.Description
		if d.Parameters != nil {
			params = d.Parameters
		}
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc, "parameters": params,
	}}
}

// ---------- 会话/项目 id（上游校验布局） ----------

const opencodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// opencodeSessionID 复刻官方 Identifier.descending() 布局：
// "ses_" + 12 位降序时间戳 hex + 14 位 Base62。
// 同一会话信号必须得到稳定同值（上游按会话做 prompt cache 亲和）。
func opencodeSessionID(signal string) string {
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	// 6 字节哈希当 36 位时间戳，左移 12 位并置 counter=1，再按位取反。
	// 取反后低 12 位是 0xFFE，落在上游接受的 counter 1-63 窗口内。
	ts := (uint64(sum[0])<<28 | uint64(sum[1])<<20 | uint64(sum[2])<<12 |
		uint64(sum[3])<<4 | uint64(sum[4])>>4) << 12
	inverted := ^(ts | 1) & 0xFFFFFFFFFFFF
	timePart := hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
	// 后 10 字节 → 定宽 14 位 Base62（左侧补 '0'，与官方一致）。
	n := new(big.Int).SetBytes(sum[6:16])
	base := big.NewInt(62)
	out := make([]byte, 14)
	rem := new(big.Int)
	for i := 13; i >= 0; i-- {
		n.DivMod(n, base, rem)
		out[i] = opencodeBase62[rem.Int64()]
	}
	return "ses_" + timePart + string(out)
}

// opencodeProjectID 裸 40 位小写 hex（官方 prj_* 规范 id 会被免费档拒）。
func opencodeProjectID(project string) string {
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	return hex.EncodeToString(sum[:20])
}

// ---------- 代理本体 ----------

// OpencodeProxy OpenCode 免费线路的本地代理。
type OpencodeProxy struct {
	Port int
	log  *FileLogger

	mu    sync.Mutex
	ln    net.Listener
	cli   *http.Client
	portS *http.Server
}

func NewOpencodeProxy() *OpencodeProxy {
	return &OpencodeProxy{Port: OpencodePort, log: NewFileLogger(dataPath("converter-opencode.log"))}
}

func (p *OpencodeProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

func (p *OpencodeProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	ln, err := listenLinePort(p.Port)
	if err != nil {
		return false, fmt.Sprintf("端口 %d 占用：%v", p.Port, err)
	}
	p.mu.Lock()
	p.ln = ln
	p.cli = newBackendClient() // 走系统/环境代理：免费档有区域校验
	p.portS = newLineServer(p)
	p.mu.Unlock()

	go func() {
		if e := p.portS.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			p.log.Log("serve error: %v", e)
		}
	}()
	p.log.Log("==== opencode 免费线路启动（%d 个免费模型）====", len(opencodeFreeModels))
	return true, fmt.Sprintf("已启动（OpenCode 免费档 · %d 个模型）", len(opencodeFreeModels))
}

func (p *OpencodeProxy) Stop() bool {
	p.mu.Lock()
	srv, ln := p.portS, p.ln
	p.portS, p.ln = nil, nil
	p.mu.Unlock()
	closeLineServer(srv, ln)
	_ = waitPortFree(p.Port, lineShutdownGrace)
	return false
}

func (p *OpencodeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "backend": opencodeBaseURL, "models": len(opencodeFreeModels)})
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

// handleModels 模型目录。DSH 等客户端读 name 当显示名、读 contextWindow /
// max_output_tokens 当容量，所以「免费」标注与上下文都必须写进 name 和这两个字段。
func (p *OpencodeProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("client_version") != "" {
		ids := make([]string, 0, len(opencodeFreeModels))
		for _, m := range opencodeFreeModels {
			ids = append(ids, m.ID)
		}
		writeJSON(w, http.StatusOK, codexModelsResponse(ids))
		return
	}
	data := make([]map[string]any, 0, len(opencodeFreeModels))
	for _, m := range opencodeFreeModels {
		data = append(data, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "opencode-free",
			"name":     fmt.Sprintf("%s · 免费 · %s", m.Name, opencodeCtxLabel(m.Context, m.Output)),
			// 结构化容量：支持这些字段的客户端（DSH）直接读。
			"contextWindow":     m.Context,
			"max_input_tokens":  m.Context,
			"max_output_tokens": m.Output,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// opencodeCtxLabel 把 token 数写成人类可读的上下文标注（如 上下文 1M/输出 128K）。
//
// 单位判定按「先整除二进制、再整除十进制」的顺序：1048576 要读成 1M 而不是
// 1024K，262144 要读成 256K 而不是 0.25M；上游两种口径的 limit 都在用
// （1048576 与 1000000 都出现过）。
func opencodeCtxLabel(ctx, out int) string {
	one := func(n int) string {
		switch {
		case n >= 1048576 && n%1048576 == 0:
			return fmt.Sprintf("%dM", n/1048576)
		case n >= 1000000 && n%1000000 == 0:
			return fmt.Sprintf("%dM", n/1000000)
		case n >= 1024 && n%1024 == 0:
			return fmt.Sprintf("%dK", n/1024)
		case n >= 1000 && n%1000 == 0:
			return fmt.Sprintf("%dK", n/1000)
		default:
			return fmt.Sprint(n)
		}
	}
	if ctx == 0 {
		return "上下文未知"
	}
	if out == 0 {
		return "上下文 " + one(ctx)
	}
	return "上下文 " + one(ctx) + "/输出 " + one(out)
}

// handleChat 转发到 opencode 免费档。
//
// 上游只接受流式：客户端要流式就原样管道，要非流式就把流收完聚合成
// chat.completion JSON。
func (p *OpencodeProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	cli := p.cli
	p.mu.Unlock()

	var raw map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad json: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	// 模型校验放在「线路是否已启动」之前：非免费模型是永久性输入错误，
	// 不该因为线路没起来而被误报成 503（用户也就看不出是自己填错了模型）。
	model, _ := raw["model"].(string)
	t0 := time.Now() // 用量统计要记耗时，口径与其它线路一致
	meta, ok := opencodeModelIDs[model]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "opencode 线路只挂免费模型，未知模型：" + model +
					"（可用：" + strings.Join(opencodeModelIDList(), ", ") + "）",
				"type": "invalid_request_error", "code": "unknown_model"}})
		return
	}
	// 上游按模型限制协议：这类模型只收 responses，别让 chat 请求撞上去拿到
	// 一句看不懂的 ModelProtocolUnsupported。
	if meta.NoChat {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "模型 " + model + " 在 opencode 上游只支持 responses 协议，请走 /v1/responses",
				"type": "invalid_request_error", "code": "protocol_unsupported"}})
		return
	}
	if cli == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "线路未启动", "type": "server_error"}})
		return
	}
	wantStream, _ := raw["stream"].(bool)
	body := opencodeBuildBody(raw)

	b, _ := json.Marshal(body)
	req, err := p.newOpencodeRequest(r.Context(), "/v1/chat/completions", model, b)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "server_error"}})
		return
	}

	resp, err := cli.Do(req)
	if err != nil {
		p.log.Log("网络错误 | %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		p.opencodeUpstreamError(w, resp, "", model)
		return
	}

	if wantStream {
		usage := opencodePipeStream(w, resp.Body)
		recordOpencodeUsage(model, "chat", 200, time.Since(t0).Milliseconds(), usage)
		return
	}
	agg, err := opencodeCollectStream(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	writeJSON(w, http.StatusOK, agg)
	var u map[string]any
	if m, ok := agg["usage"].(map[string]any); ok {
		u = m
	}
	recordOpencodeUsage(model, "chat", 200, time.Since(t0).Milliseconds(), u)
}

// opencodeModelIDList 供错误提示列出可用模型。
func opencodeModelIDList() []string {
	out := make([]string, 0, len(opencodeFreeModels))
	for _, m := range opencodeFreeModels {
		out = append(out, m.ID)
	}
	return out
}

// newOpencodeRequest 组装一个打向上游免费档的请求，把免费档闸门要求的一切都备齐。
// path 传 "/v1/chat/completions" 或 "/v1/responses"。
func (p *OpencodeProxy) newOpencodeRequest(ctx context.Context, path, model string, body []byte) (*http.Request, error) {
	// 会话亲和：同一串消息稳定映射到同一会话，命中上游 prompt cache。
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	sid := opencodeSessionID(model + "\x00" + string(head))
	pid := opencodeProjectID("aihub-opencode")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opencodeBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "opencode/latest/"+opencodeClientVer+"/cli")
	req.Header.Set("x-opencode-client", "cli")
	// 四个会话头同值，缺一个是 403。
	req.Header.Set("x-opencode-session", sid)
	req.Header.Set("x-opencode-session-id", sid)
	req.Header.Set("x-session-affinity", sid)
	req.Header.Set("X-Session-Id", sid)
	req.Header.Set("x-opencode-project", pid)
	req.Header.Set("Authorization", "Bearer "+opencodeAnonKey)
	// 刻意不设 x-opencode-request：2026-10-14 起该头被上游拒。
	return req, nil
}

// opencodeUpstreamError 把上游错误翻成 client 看得懂的 message + 可操作的 hint。
// label 非空时用作日志前缀（区分 chat / responses 两条路径）。
func (p *OpencodeProxy) opencodeUpstreamError(w http.ResponseWriter, resp *http.Response, label, model string) {
	nb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	msg := string(nb)
	code := ""
	var ej struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(nb, &ej) == nil && ej.Error.Message != "" {
		msg = ej.Error.Message
		code = ej.Error.Type
	}
	hint := ""
	switch {
	case strings.Contains(msg, "free tier") || strings.Contains(code, "FreeTier"):
		hint = "（免费档闸门：请求必须流式且带官方核心工具定义，见 opencode_proxy.go 顶部）"
	case strings.Contains(msg, "not available in your country"):
		hint = "（区域限制：需要海外出口，检查系统代理或 HTTP(S)_PROXY 是否可用）"
	case strings.Contains(code, "ModelProtocolUnsupported"):
		hint = "（该模型不支持此协议，换另一条路径：chat 与 responses 的上游支持面按模型互补）"
	}
	// 上游经常**只回状态码、body 全空**（2026-10-18 实测 deepseek-v4.1-flash-free /
	// step-5-preview-free 稳定 HTTP 500 无 body）。原样转发的话，客户端拿到的是
	// {"code":"","message":"","type":"upstream_error"} —— 什么信息都没有，
	// dsh 里表现为「模型莫名不好用」且重试也救不了。这里必须替上游把话说完整。
	if strings.TrimSpace(msg) == "" {
		msg = fmt.Sprintf("上游 opencode.ai 对模型 %s 返回 HTTP %d 且没有响应体。"+
			"这通常意味着该模型已从免费档下架或上游侧故障，重试无效；", model, resp.StatusCode)
		if hint == "" {
			hint = "请改用同线路的其它免费模型，或稍后再试。"
		}
	}
	if label != "" {
		p.log.Log("%s HTTP %d | %s", label, resp.StatusCode, truncate(msg, 400))
	} else {
		p.log.Log("HTTP %d | %s", resp.StatusCode, truncate(msg, 400))
	}
	writeJSON(w, resp.StatusCode, map[string]any{
		"error": map[string]any{"message": msg + hint, "type": "upstream_error", "code": code}})
}

// opencodeBuildBody 把客户端请求整形成免费档能收的形状：
// 强制流式 + 补齐官方核心工具（保留客户端自己的工具）。
func opencodeBuildBody(raw map[string]any) map[string]any {
	body := map[string]any{}
	for _, k := range []string{"model", "messages", "temperature", "top_p", "stop",
		"presence_penalty", "frequency_penalty", "response_format", "seed", "user",
		"reasoning_effort", "max_tokens", "max_completion_tokens", "tool_choice"} {
		if v, ok := raw[k]; ok {
			body[k] = v
		}
	}
	body["stream"] = true // 上游硬要求
	if _, ok := body["messages"]; !ok {
		body["messages"] = []any{}
	}

	tools := make([]any, 0, len(opencodeCoreTools)+4)
	seen := map[string]bool{}
	if have, ok := raw["tools"].([]any); ok {
		for _, it := range have {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			n := opencodeToolName(m)
			if n != "" {
				seen[n] = true
			}
			tools = append(tools, it)
		}
	}
	// 客户端若声明了同名工具，一律换成官方定义：上游会按官方定义逐字节比对，
	// 客户端自带的简化描述会触发 403。
	for _, n := range opencodeCoreTools {
		if seen[n] {
			tools = opencodeReplaceTool(tools, n)
			continue
		}
		tools = append(tools, opencodeCoreToolWire(n))
		seen[n] = true
	}
	body["tools"] = tools
	return body
}

// opencodeToolName 取工具定义的 name（chat 形态嵌在 function 里）。
func opencodeToolName(m map[string]any) string {
	if f, ok := m["function"].(map[string]any); ok {
		if n, ok := f["name"].(string); ok && n != "" {
			return n
		}
	}
	n, _ := m["name"].(string)
	return n
}

// opencodeReplaceTool 把 tools 里名为 name 的那条原地换成官方定义。
func opencodeReplaceTool(tools []any, name string) []any {
	for i, it := range tools {
		if m, ok := it.(map[string]any); ok && opencodeToolName(m) == name {
			tools[i] = opencodeCoreToolWire(name)
		}
	}
	return tools
}

// opencodePipeStream 把上游 SSE 原样转给客户端（标准 OpenAI chunk，无需改写）。
// opencodePipeStream 原样转发上游 SSE，同时把最后一个 usage 对象捞出来返回。
// 捞它不额外花成本：这里本来就在逐行扫。返回的 usage 交给 recordOpencodeUsage
// 记账 —— 用量统计原先只在聚合口记（见 recordTap），直连 :8800 的请求一行都不写，
// 控制台「用量统计」里 OpenCode 免费档因此是空的（2026-10-16 用户问起，实测台账
// 里 oc 计数为 0 确认）。
func opencodePipeStream(w http.ResponseWriter, body io.Reader) map[string]any {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var usage map[string]any
	for sc.Scan() {
		line := sc.Text()
		// 只有带 usage 的 data 行才值得解一次 JSON。
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "usage") {
			var probe struct {
				Usage map[string]any `json:"usage"`
				Response struct {
					Usage map[string]any `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &probe) == nil {
				if probe.Usage != nil {
					usage = probe.Usage
				} else if probe.Response.Usage != nil {
					usage = probe.Response.Usage
				}
			}
		}
		fmt.Fprintln(w, line)
		if fl != nil {
			fl.Flush()
		}
	}
	// 2026-10-21 开源审查修：此前从不检查 sc.Err()。单条 SSE 行超过
	// sc.Buffer 上限（4MB）时 Scan() 返回 false，循环安静结束 —— 客户端
	// 收到的是被截断的流，既没有 [DONE] 也没有任何日志。
	if err := sc.Err(); err != nil {
		logHub("[opencode] ✗ 上游流扫描中断（可能单行超 4MB 缓冲）: %v", err)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	return usage
}

// recordOpencodeUsage 给 OpenCode 免费档落一行用量。
// kind 传 "chat" / "responses"，code 为 0 时按 200 记。
func recordOpencodeUsage(model, kind string, code int, ms int64, usage map[string]any) {
	in, out, cached, reason, total := usageFrom(usage)
	if code == 0 {
		code = 200
	}
	recordUsage(usageRec{
		Line: "oc", Slug: "oc." + model, Backend: model, Kind: kind,
		Code: code, MS: ms, In: in, Out: out, Cached: cached, Reason: reason, Total: total,
	})
}

// opencodeCollectStream 把上游 SSE 收成一条完整 chat.completion。
func opencodeCollectStream(body io.Reader) (map[string]any, error) {
	var (
		id, model string
		created   json.Number
		usage     map[string]any
		content   strings.Builder
		reasoning strings.Builder
		calls     = map[int]map[string]any{}
		order     []int
		finish    string
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			ID      string         `json:"id"`
			Model   string         `json:"model"`
			Created json.Number    `json:"created"`
			Usage   map[string]any `json:"usage"`
			Choices []struct {
				Delta *struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.ID != "" {
			id = ev.ID
		}
		if ev.Model != "" {
			model = ev.Model
		}
		if ev.Created != "" {
			created = ev.Created
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		for _, ch := range ev.Choices {
			if d := ch.Delta; d != nil {
				content.WriteString(d.Content)
				reasoning.WriteString(d.ReasoningContent)
				for _, tc := range d.ToolCalls {
					slot, ok := calls[tc.Index]
					if !ok {
						slot = map[string]any{"id": tc.ID, "type": "function",
							"function": map[string]any{"name": "", "arguments": ""}}
						calls[tc.Index] = slot
						order = append(order, tc.Index)
					}
					if tc.ID != "" {
						slot["id"] = tc.ID
					}
					f := slot["function"].(map[string]any)
					if tc.Function.Name != "" {
						f["name"] = tc.Function.Name
					}
					f["arguments"] = f["arguments"].(string) + tc.Function.Arguments
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		list := make([]any, 0, len(order))
		for _, i := range order {
			list = append(list, calls[i])
		}
		msg["tool_calls"] = list
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	ct, _ := created.Int64()
	if ct == 0 {
		ct = time.Now().Unix()
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": ct, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

// ---------- /v1/responses 直通 ----------

// opencodeCoreToolFlat 把核心工具渲染成 responses 协议形态（扁平 name，
// 不嵌 function）。上游同样要求逐字节的官方定义。
func opencodeCoreToolFlat(name string) map[string]any {
	desc := "Agent tool " + name
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	if d, ok := opencodeTools()[name]; ok {
		desc = d.Description
		if d.Parameters != nil {
			params = d.Parameters
		}
	}
	return map[string]any{"type": "function", "name": name,
		"description": desc, "parameters": params}
}

// opencodePrepareResponsesBody 整形 responses 请求体：强制流式 + 补官方核心工具。
// 上游对工具定义逐字节比对，所以客户端若声明了同名工具一律换成官方定义。
func opencodePrepareResponsesBody(raw map[string]any) map[string]any {
	body := map[string]any{}
	for k, v := range raw {
		body[k] = v
	}
	body["stream"] = true
	seen := map[string]bool{}
	arr := make([]any, 0, len(opencodeCoreTools)+4)
	if have, ok := body["tools"].([]any); ok {
		for _, it := range have {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if n := opencodeToolName(m); n != "" {
				seen[n] = true
			}
			arr = append(arr, it)
		}
	}
	for i, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if n := opencodeToolName(m); n != "" && opencodeIsCoreTool(n) {
			arr[i] = opencodeCoreToolFlat(n)
		}
	}
	for _, n := range opencodeCoreTools {
		if seen[n] {
			continue
		}
		arr = append(arr, opencodeCoreToolFlat(n))
	}
	body["tools"] = arr
	return body
}

func opencodeIsCoreTool(name string) bool {
	for _, n := range opencodeCoreTools {
		if n == name {
			return true
		}
	}
	return false
}

// handleResponses 把 /v1/responses 直通到上游同名端点。
//
// 存在的理由：上游按模型限制协议，muse-spark 只收 responses（chat 会被拒）。
// 客户端要非流式时照样强制流式、再把流收完聚合成 response 对象。
func (p *OpencodeProxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	cli := p.cli
	p.mu.Unlock()

	var raw map[string]any
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<20))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "bad json: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	model, _ := raw["model"].(string)
	t0 := time.Now() // 同上：用量统计记耗时
	meta, ok := opencodeModelIDs[model]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "opencode 线路只挂免费模型，未知模型：" + model +
					"（可用：" + strings.Join(opencodeModelIDList(), ", ") + "）",
				"type": "invalid_request_error", "code": "unknown_model"}})
		return
	}
	if meta.NoResp {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": "模型 " + model + " 在 opencode 上游只支持 chat 协议，请走 /v1/chat/completions",
				"type": "invalid_request_error", "code": "protocol_unsupported"}})
		return
	}
	if cli == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "线路未启动", "type": "server_error"}})
		return
	}
	wantStream, _ := raw["stream"].(bool)
	body := opencodePrepareResponsesBody(raw)
	b, _ := json.Marshal(body)

	req, err := p.newOpencodeRequest(r.Context(), "/v1/responses", model, b)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "server_error"}})
		return
	}
	resp, err := cli.Do(req)
	if err != nil {
		p.log.Log("responses 网络错误 | %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.opencodeUpstreamError(w, resp, "responses", model)
		return
	}
	if wantStream {
		usage := opencodePipeStream(w, resp.Body)
		recordOpencodeUsage(model, "responses", 200, time.Since(t0).Milliseconds(), usage)
		return
	}
	agg, err := opencodeCollectResponsesStream(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	writeJSON(w, http.StatusOK, agg)
	var u map[string]any
	if m, ok := agg["usage"].(map[string]any); ok {
		u = m
	}
	recordOpencodeUsage(model, "responses", 200, time.Since(t0).Milliseconds(), u)
}

// opencodeCollectResponsesStream 把上游 responses SSE 收成一个 response 对象。
// 优先用 response.completed 事件里上游自己给的完整对象；没有就从增量事件拼。
func opencodeCollectResponsesStream(body io.Reader) (map[string]any, error) {
	var (
		final   map[string]any
		text    strings.Builder
		reason  strings.Builder
		calls   = map[string]map[string]any{}
		order   []string
		respID  string
		created json.Number
		usage   map[string]any
	)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		et, _ := ev["type"].(string)
		switch et {
		case "response.completed":
			if r, ok := ev["response"].(map[string]any); ok {
				final = r
			}
		case "response.created", "response.in_progress":
			if r, ok := ev["response"].(map[string]any); ok {
				if s, _ := r["id"].(string); s != "" {
					respID = s
				}
				if c, ok := r["created_at"].(json.Number); ok {
					created = c
				}
			}
		case "response.output_text.delta":
			text.WriteString(mapString(ev["delta"]))
		case "response.reasoning_summary_text.delta":
			reason.WriteString(mapString(ev["delta"]))
		case "response.output_item.added", "response.output_item.done":
			item, _ := ev["item"].(map[string]any)
			if item == nil {
				break
			}
			if t, _ := item["type"].(string); t == "function_call" {
				key, _ := item["call_id"].(string)
				if key == "" {
					key, _ = item["id"].(string)
				}
				if key == "" {
					key = fmt.Sprint(len(order))
				}
				if _, ok := calls[key]; !ok {
					order = append(order, key)
				}
				calls[key] = item
			}
		case "response.function_call_arguments.delta":
			key := mapString(ev["call_id"])
			if key == "" {
				key = mapString(ev["item_id"])
			}
			if item, ok := calls[key]; ok {
				item["arguments"] = mapString(item["arguments"]) + mapString(ev["delta"])
			}
		case "response.usage", "response.response.usage":
			if u, ok := ev["usage"].(map[string]any); ok {
				usage = u
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if final != nil {
		return final, nil
	}
	output := make([]any, 0, len(order)+2)
	if s := reason.String(); s != "" {
		output = append(output, map[string]any{"type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": s}}})
	}
	if t := text.String(); t != "" {
		output = append(output, map[string]any{"type": "message", "role": "assistant",
			"status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": t, "logprobs": []any{}}}})
	}
	for _, k := range order {
		output = append(output, calls[k])
	}
	ct, _ := created.Int64()
	if ct == 0 {
		ct = time.Now().Unix()
	}
	out := map[string]any{
		"id": respID, "object": "response", "created_at": ct,
		"status": "completed", "output": output,
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

func mapString(v any) string {
	s, _ := v.(string)
	return s
}
