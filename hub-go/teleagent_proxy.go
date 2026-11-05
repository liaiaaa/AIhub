package main

// TeleAgent（TeleAI 桌面客户端）线路（2026-11-02）。
//
// 上游是标准 OpenAI 兼容接口：
//
//	POST https://agent.teleai.com.cn/superCowork/sapi/api/v1/chat/completions
//
// 但它不是「带个 Bearer 就能用」，网关要求一整套 super agent 安全头，其中
// X-SuperAgent-Signature 是**真验证**的 HMAC-SHA256（实测：缺头 40101、
// 缺版本 40104、签名错 40104 invalid signature）。
//
// 签名算法（反汇编 + 开源实现 hj01857655/TeleAgent2api 互证，2026-11-02 实测 200）：
//
//	derivedKey = hexHMAC(key = JWT 第三段,       // 注意：用第三段当 HMAC 的 key
//	                     msg = "superagent-auth-v1/<token>/<installId>/<ts>/<nonce>")
//	bodySHA    = hexSHA256(body)
//	requestStr = join([
//	       "superagent-auth-v1", "POST", pathWithQuery, ts, nonce, appVersion, bodySHA
//	                     ], "\n")
//	signature  = hexHMAC(key = derivedKey, msg = requestStr)
//
// 其中 ts 是**秒**级 unix 时间戳，nonce 是 UUIDv4，appVersion 形如 3.0.0-rc.1。
// deviceId / installId 来自 Electron 的 `~/.local/share/TeleAgent/app-auth/device-meta.json`，
// token 由 teleagent_vault.go 从 safeStorage 解密。
//
// 提醒：这套头是逆向上游得来的（非公开 API），上游改版就会失效；失效时表现为
// 40101/40104，重跑一次上面的探测即可判断是哪一环变了。

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"time"
)

// TeleAgentPort 本线路本地端口（8794：紧邻 trae 的 8795，未占用）。
const TeleAgentPort = 8794

const (
	teleagentUpstreamBase = "https://agent.teleai.com.cn/superCowork/sapi/api/v1"
	teleagentChatPath     = "/superCowork/sapi/api/v1/chat/completions"
	teleagentSignPrefix   = "superagent-auth-v1"
	teleagentSignVersion  = "v1"
	// 上游对非官方客户端敏感，固定成一个真实存在过的版本号；env 可覆盖。
	teleagentDefaultAppVersion = "3.0.0-rc.1"
)

// teleagentModels TeleAgent 可用模型（来自应用自己的 model-catalog.json + 源码种子）。
// 只写确定的：上下文/输出上限不确定的留 0，不编数字。
//
// 前台目录 vs 隐藏项（2026-11-03 用户指正「teleagent 应该就三个模型才对」）：
// TeleAgent 桌面端模型选择器只展示 智能/旗舰/极速 三个（model-catalog.json 里也只有这三个）；
// chat-lite / chat-nano 只存在于 TeleAgent.jsonc 的 provider.limits，前台不展示。
// 所以对外目录（/v1/models、聚合可选清单、控制台下拉）**只列前台三个**；
// 隐藏的两个仍可直接按 id 调用（实测 200），需要时把 Hidden 改 false 即可放出。
var teleagentModels = []struct {
	ID       string
	Display  string
	Context  int
	MaxOut   int
	Disabled bool // 应用侧 blacklist，彻底不对外
	Hidden   bool // 前台目录不展示：不进 /v1/models 与聚合清单，但按 id 直连仍可用
}{
	// 名字取自应用自己的 model-catalog.json（前台只展示这三个）。
	// 上限：model-catalog.json 写 400,000，而 TeleAgent.jsonc 的 provider.limits 写
	// 1,000,000 —— 2026-11-02 用户确认「模型是支持 1M 上下文的」，按 1M 宣告。
	{ID: "chat-pro", Display: "智能", Context: 934000, MaxOut: 65536},
	{ID: "chat-flagship", Display: "旗舰", Context: 934000, MaxOut: 65536},
	{ID: "chat-flash", Display: "极速", Context: 934000, MaxOut: 65536},
	// 上下文按 934000 报：官方目录写 400000，但用户实际可用约 1M，
	// 取 934k 是**留出余量**（2026-11-03 用户明确「按照 934k 来就行，预留出来点」）。
	// 轻量：**是桌面端的识图模型**（runtime.js 里 `smallModelRef = "NewApi/chat-lite"`，
	// 图片识别、摘要这类小活都走它）。2026-11-03 实测它能准确读出截图里的文字与控件，
	// 所以从「隐藏」放出来，并作为 aihub 图片转文字的首选视觉模型。
	{ID: "chat-lite", Display: "轻量", Context: 100000, MaxOut: 16384},
	{ID: "chat-nano", Display: "轻速", Context: 100000, MaxOut: 16384, Hidden: true},
	// 应用自己把它放在 blacklist 里，不对外暴露
	{ID: "tele-safety-answer", Display: "安全审核", Disabled: true},
}

// teleagentPublicModels 对外目录（排除 Disabled 与 Hidden）。
func teleagentPublicModels() []string {
	out := make([]string, 0, 3)
	for _, m := range teleagentModels {
		if !m.Disabled && !m.Hidden {
			out = append(out, m.ID)
		}
	}
	return out
}

// TeleAgentProxy 一条线路一个实例。
type TeleAgentProxy struct {
	Port int

	mu    sync.Mutex
	port  int
	srv   *http.Server
	ln    net.Listener
	cli   *http.Client
	log   *FileLogger
	token string
	devID string
	insID string
}

// NewTeleAgentProxy 构造线路（端口来自 lines.go）。
func NewTeleAgentProxy(port int) *TeleAgentProxy {
	return &TeleAgentProxy{Port: port, port: port}
}

func (p *TeleAgentProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

// Start 启动线路：先确认凭据可读，再监听端口。
func (p *TeleAgentProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	token, err := teleagentToken()
	if err != nil {
		return false, "TeleAgent 凭据不可用：" + err.Error()
	}
	dev, ins, err := teleagentDeviceMeta()
	if err != nil {
		return false, "TeleAgent 设备信息不可用：" + err.Error()
	}
	ln, err := listenLinePort(p.Port)
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", p.Port, err)
	}
	p.mu.Lock()
	p.token, p.devID, p.insID = token, dev, ins
	p.cli = &http.Client{Transport: directClientFor("teleagent").Transport, Timeout: 0}
	p.ln = ln
	p.srv = newLineServerFor("teleagent", p)
	srv := p.srv
	p.mu.Unlock()

	if p.log == nil {
		p.log = NewFileLogger(dataPath("converter-teleagent.log"))
	}
	go func() {
		defer recoverGoroutine("teleagent_proxy.go:Start")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.Log("serve error: %v", err)
		}
	}()
	nPub := len(teleagentPublicModels())
	p.log.Log("==== proxy 启动（%d，模型 %d 个）====", p.Port, nPub)
	return true, "已启动（TeleAgent · " + fmt.Sprint(nPub) + " 个模型）"
}

func (p *TeleAgentProxy) Stop() (bool, string) {
	p.mu.Lock()
	srv, ln := p.srv, p.ln
	p.srv, p.ln = nil, nil
	p.mu.Unlock()
	if srv != nil && ln != nil {
		closeLineServer(srv, ln)
	}
	if waitPortFree(p.Port, lineShutdownGrace) {
		return true, "已停止"
	}
	return false, "TeleAgent 停止失败"
}

func (p *TeleAgentProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "line": "teleagent"})
	case "/v1/models", "/models":
		p.handleModels(w, r)
	case "/v1/chat/completions", "/chat/completions":
		p.handleChat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *TeleAgentProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	data := make([]map[string]any, 0, len(teleagentModels))
	for _, m := range teleagentModels {
		if m.Disabled || m.Hidden {
			continue // 前台目录不展示的模型不进列表
		}
		e := map[string]any{
			"id": m.ID, "object": "model", "created": now, "owned_by": "teleagent",
			"name": m.Display,
		}
		if m.Context > 0 {
			e["contextWindow"] = m.Context
			e["context_window"] = m.Context
			e["max_input_tokens"] = m.Context
		}
		if m.MaxOut > 0 {
			e["max_output_tokens"] = m.MaxOut
		}
		data = append(data, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleChat 转发一次 chat/completions。
// teleagentNormalizeEffort 把客户端档位折成上游认的三个值（low/high/max）。
//
// 2026-11-03 实测：上游网关只接受 low/high/max；传 minimal 会 **400**
// 「该模型始终思考，不支持关闭思考；请使用 low、high 或 max」。
// 而客户端（Codex/DSH）看的是规范档位（minimal/medium/xhigh…），所以必须在这里折：
//
//	minimal → low（上游不允许关思考，只能取最轻）
//	medium  → high
//	xhigh   → max
//
// 已经合法的值原样保留；没有该字段就不动。
func teleagentNormalizeEffort(m map[string]any) map[string]any {
	v, ok := m["reasoning_effort"]
	if !ok {
		return m
	}
	want := lowerTrim(mapStr(v))
	if wire, ok := canonicalToWireEffort("teleagent", want); ok {
		m["reasoning_effort"] = wire
	} else if want == "" {
		delete(m, "reasoning_effort")
	} else {
		// 认不出的值：删掉，交给上游默认（比发一个必然 400 的值好）
		delete(m, "reasoning_effort")
	}
	return m
}

func (p *TeleAgentProxy) handleChat(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	rid := randHex(4)
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "读取请求体失败：" + err.Error()}})
		return
	}
	// 模型名去掉线路前缀（teleagent.chat-flash → chat-flash）
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	backend := strings.TrimPrefix(probe.Model, "teleagent.")
	// 统一处理请求体：去掉线路前缀、归一化思考档位（两件事都只在 JSON 能解析时做）。
	// 解析失败就原样转发 —— 上游自己会给出更准确的错误。
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		payload["model"] = backend
		teleagentNormalizeEffort(payload)
		teleagentNormalizeMessages(payload)
		// 图片一律先转文字 —— 与桌面端行为一致（见 teleagent_vision.go），同时也避开
		// 「原生吃图但极慢」的路径：2026-11-03 实测同一张 2092×918 截图，
		// teleagent.chat-lite 1.4s、teleagent.chat-pro 79.4s；用户就是等不及 79s 才终止任务的。
		// 输出预算下限（2026-11-04 实测）：TeleAgent 的模型都是「先思考再答」，
		// max_tokens 给太小会把预算全花在 reasoning 上，正文 0 字、finish_reason=length，
		// 客户端看到的就是「空回答」然后无限重试。实测同一请求：
		//   max_tokens=60   → 28.9s，reasoning 110 字，正文 0 字（finish=length）
		//   max_tokens=300  → 1.5s，正文 77 字（finish=stop）
		// 所以这里兜一个下限，客户端要得比它小就抬上来（可用 AIHUB_TELEAGENT_MIN_TOKENS 调）。
		if mt := teleagentMaxTokens(payload); mt > 0 && mt < teleagentMinTokens() {
			payload["max_tokens"] = teleagentMinTokens()
			p.log.Log("[%s] max_tokens %d 太小（会全花在思考上、正文为空）→ 抬到 %d", rid, mt, teleagentMinTokens())
		}
		// 图片处理策略（2026-11-04 用户反馈「旗舰看不见图，只能用 OCR」后改成 auto）：
		//   auto（默认）：**能吃图的模型（chat-pro / chat-flash / chat-lite）原样透传图片**，
		//                让它真正「看图」；只有纯文本模型（chat-flagship / chat-nano）才提前转文字。
		//   always      ：一律先转文字（2026-11-03 那版行为，为绕开 chat-pro 原生吃图 79 秒）。
		// 透传后被上游 400 的话，下面还有「转文字重发 → 去图重试」两级兜底。
		if teleagentVisionEnabled() && hasImageParts(payload) &&
			(teleagentVisionMode() == "always" || !modelSupportsImage("teleagent", backend)) {
			if n, ok := replaceImagesWithDescriptions(payload, func(format string, args ...any) {
				p.log.Log("[%s] "+format, append([]any{rid}, args...)...)
			}); ok && n > 0 {
				p.log.Log("[%s] %s：%d 张图已转成文字（该模型不吃图，按桌面端做法先识图）", rid, backend, n)
			}
		}
		if b, err := json.Marshal(payload); err == nil {
			body = b
		}
	}

	resp, err := p.doUpstream(r, body, rid)
	if err != nil {
		p.log.Log("[%s] ✗ %v", rid, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// token 可能被轮换：清缓存重试一次
		teleagentForgetToken()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp2, err2 := p.doUpstream(r, body, rid); err2 == nil && resp2.StatusCode != http.StatusUnauthorized {
			resp = resp2
			defer resp2.Body.Close()
		}
	}

	// 上游非 2xx：把响应体记进线路日志，并把可读错误回给客户端。
	// 2026-11-02 用户报「显示几个思考后出现的报错」——先保证下次有据可查。
	if resp.StatusCode >= 400 {
		body4err, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		// 兜底（2026-11-03）：图片形态仍然被上游拒时，去掉图片再试一次 ——
		// 宁可回一句「图片没转过去」，也不要整轮对话失败。
		// 2026-11-03：桌面端处理图片的方式是「先调视觉模型转成文字」，不是把图塞给对话模型
		// （证据见 teleagent_vision.go 顶部注释）。这里照做：先把图转成文字描述，
		// 再以纯文本重发；转换成功就开始一次「带描述」的新请求，失败才退到去图。
		if hasImageParts(payload) && teleagentVisionEnabled() {
			if n, ok := replaceImagesWithDescriptions(payload, func(format string, args ...any) {
				p.log.Log("[%s] "+format, append([]any{rid}, args...)...)
			}); ok && n > 0 {
				if b2, err2 := json.Marshal(payload); err2 == nil {
					if resp2, err3 := p.doUpstream(r, b2, rid); err3 == nil {
						resp.Body.Close()
						resp = resp2
						body4err = nil
						if resp.StatusCode < 400 {
							p.log.Log("[%s] ◀ 图片已转成文字后重发成功（%d 张）", rid, n)
							goto relay
						}
						b2err, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
						resp.Body.Close()
						body4err = b2err
					}
				}
			}
		}
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(body4err), "content.type") && hasImageParts(payload) {
			p.log.Log("[%s] ⚠ 图片请求被上游拒：%s | 拒收前结构：%s", rid,
				strings.TrimSpace(string(body4err)), teleagentPayloadShape(payload))
			// 再记一份「图片 part 的字段清单」：键名 + url 形态，不含 base64 正文。
			// 上游对 content 的校验比想象的细，只看 type 不够。
			for i, sample := range teleagentPartsSample(payload, 3) {
				p.log.Log("[%s] 图片 part#%d：%s", rid, i+1, sample)
			}
			if b2, ok := stripImageParts(payload); ok {
				p.log.Log("[%s] ⚠ 上游拒收图片 part，改为纯文本重试（%d 字节）", rid, len(b2))
				if resp2, err2 := p.doUpstream(r, b2, rid); err2 == nil {
					if resp2.StatusCode < 400 {
						resp.Body.Close()
						resp = resp2
						body4err = nil
						goto relay
					}
					resp2.Body.Close()
				}
			}
		}
		// 2026-11-03 用户报「chat-pro 发图 400 取值范围 ['text']」，但同参数手工探测却 200 ——
		// 光记错误体查不出差别，所以把**请求结构**（消息数、角色、每个 content part 的 type）
		// 一起记下来。只记类型与长度，不记正文。
	relay:
		mark := "✗"
		if resp.StatusCode < 400 {
			mark = "◀"
		}
		p.log.Log("[%s] %s HTTP %d | %s | %s", rid, mark, resp.StatusCode, backend, strings.TrimSpace(string(body4err)))
		if payload != nil {
			p.log.Log("[%s] 请求结构：%s", rid, teleagentPayloadShape(payload))
			if os.Getenv("AIHUB_TELEAGENT_DUMP") == "1" {
				_ = os.WriteFile(dataPath(fmt.Sprintf("ta-fail-%s.json", rid)), body, 0o600)
				p.log.Log("[%s] 已把完整请求体写到 ta-fail-%s.json", rid, rid)
			}
		}
		writeJSON(w, resp.StatusCode, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("TeleAgent 上游 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body4err))),
			"type":    "upstream_error",
		}})
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	written := 0
	var readErr error
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			written += n
			if _, werr := w.Write(buf[:n]); werr != nil {
				readErr = fmt.Errorf("写客户端失败：%w", werr)
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	if readErr != nil {
		p.log.Log("[%s] ⚠ 流中断（已写 %dB）：%v", rid, written, readErr)
	}
	p.log.Log("[%s] ◀ %d | %s | %.2fs | %dKB", rid, resp.StatusCode, backend,
		time.Since(t0).Seconds(), len(body)/1024)
}

func (p *TeleAgentProxy) doUpstream(r *http.Request, body []byte, rid string) (*http.Response, error) {
	p.mu.Lock()
	token, dev, ins, cli := p.token, p.devID, p.insID, p.cli
	p.mu.Unlock()
	if token == "" {
		var err error
		if token, err = teleagentToken(); err != nil {
			return nil, err
		}
	}
	if dev == "" || ins == "" {
		var err error
		if dev, ins, err = teleagentDeviceMeta(); err != nil {
			return nil, err
		}
	}
	if cli == nil {
		cli = &http.Client{Transport: directClientFor("teleagent").Transport, Timeout: 0}
	}

	headers, err := teleagentSignHeaders(token, dev, ins, http.MethodPost, teleagentChatPath, body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		teleagentUpstreamBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	return cli.Do(req)
}

// teleagentDeviceMeta 读 Electron 侧的设备标识（deviceId 12 位 hex、installId UUIDv4）。
func teleagentDeviceMeta() (deviceID, installID string, err error) {
	home, herr := os.UserHomeDir()
	if herr != nil {
		return "", "", herr
	}
	path := filepath.Join(home, ".local", "share", "TeleAgent", "app-auth", "device-meta.json")
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		return "", "", fmt.Errorf("未找到 %s", path)
	}
	var m struct {
		DeviceID  string `json:"deviceId"`
		InstallID string `json:"installId"`
	}
	if jerr := json.Unmarshal(b, &m); jerr != nil {
		return "", "", jerr
	}
	if m.DeviceID == "" || m.InstallID == "" {
		return "", "", fmt.Errorf("device-meta.json 缺字段")
	}
	return m.DeviceID, m.InstallID, nil
}

// teleagentClientType 上游「用量面板 → 调用端」这一列的取值来源（2026-11-02 用户反馈）。
//
// 桌面端自己发请求时这一列显示「桌面端」；我们反代的请求原来不带这个头，面板里就是空的，
// 容易被看成「不是客户端发的」。默认按桌面色值发 desktop，env 可覆盖（若上游改了枚举）。
func teleagentClientType() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_TELEAGENT_CLIENT_TYPE")); v != "" {
		return v
	}
	return "desktop"
}

// teleagentAppVersion 上游要的客户端版本号（env 可覆盖，应用升级后可能要对齐）。
func teleagentAppVersion() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_TELEAGENT_APP_VERSION")); v != "" {
		return v
	}
	return teleagentDefaultAppVersion
}

// teleagentSignHeaders 生成整套鉴权头（纯函数，便于单测）。
func teleagentSignHeaders(token, deviceID, installID, method, pathWithQuery string, body []byte) (map[string]string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return nil, fmt.Errorf("token 不是合法 JWT")
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	nonce, err := uuidV4()
	if err != nil {
		return nil, err
	}
	return teleagentSignHeadersAt(token, deviceID, installID, method, pathWithQuery, teleagentAppVersion(), body, ts, nonce)
}

// teleagentSignHeadersAt 与上面相同，但时间戳/nonce 由调用方给（测试可复现）。
func teleagentSignHeadersAt(token, deviceID, installID, method, pathWithQuery, appVersion string, body []byte, ts, nonce string) (map[string]string, error) {
	segs := strings.Split(token, ".")
	if len(segs) != 3 || segs[2] == "" {
		return nil, fmt.Errorf("token 不是合法 JWT")
	}
	jwt3 := segs[2]
	derived := hmacSHA256Hex(jwt3, fmt.Sprintf("%s/%s/%s/%s/%s", teleagentSignPrefix, token, installID, ts, nonce))
	bodySHA := sha256Hex(body)
	requestStr := strings.Join([]string{
		teleagentSignPrefix, strings.ToUpper(method), pathWithQuery, ts, nonce, appVersion, bodySHA,
	}, "\n")
	sig := hmacSHA256Hex(derived, requestStr)

	msgID, _ := hexToken(16)
	sessID, _ := hexToken(16)
	return map[string]string{
		"X-Token":                   token,
		"X-Route-Target":            "ops-gateway",
		"X-App-Version":             appVersion,
		"X-SuperAgent-Timestamp":    ts,
		"X-SuperAgent-Nonce":        nonce,
		"X-SuperAgent-Device-Id":    deviceID,
		"X-SuperAgent-Install-Id":   installID,
		"X-SuperAgent-Sign-Version": teleagentSignVersion,
		"X-SuperAgent-Signature":    sig,
		"User-Agent":                "TeleAgent/" + appVersion,
		"Accept-Encoding":           "identity",
		"x-message-id":              "msg_" + msgID,
		"x-session-id":              "ses_" + sessID,
		// 2026-11-03 修：这个头之前只定义了函数、**从来没被发出去**
		// （teleagentClientType 全仓库零调用点），所以官网用量面板「调用端」一直是空的。
		// 桌面端自己发 desktop，我们照抄。
		"X-TeleAI-Client-Type": teleagentClientType(),
	}, nil
}

// ---------- 小工具 ----------

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Hex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

func hexToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// uuidV4 生成标准 UUIDv4（上游 nonce 要求随机 UUID）。
func uuidV4() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// teleagentPayloadShape 把请求体的**结构**压成一行：消息数、角色、content 里出现的 part 类型、
// 以及每种 part 的个数。只描述形状，不含正文（用户内容不进日志）。
func teleagentPayloadShape(payload map[string]any) string {
	msgs, _ := payload["messages"].([]any)
	roles := make([]string, 0, len(msgs))
	types := map[string]int{}
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		roles = append(roles, role)
		switch c := m["content"].(type) {
		case string:
			types["<string>"]++
		case []any:
			for _, part := range c {
				pm, _ := part.(map[string]any)
				t, _ := pm["type"].(string)
				if t == "" {
					t = "<no-type>"
				}
				types[t]++
				// image_url 这类 part 光看 type 不够：url 是 data URL 还是附件路径/URI，
				// 决定上游认不认。这里只记「什么形状」，不记 base64 正文。
				if t == "image_url" {
					iu, _ := pm["image_url"].(map[string]any)
					u, _ := iu["url"].(string)
					types[imageURLShape(u)]++
				}
			}
		case nil:
			types["<null>"]++
		default:
			types[fmt.Sprintf("<%T>", c)]++
		}
	}
	keys := make([]string, 0, len(types))
	for k := range types {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, types[k]))
	}
	return fmt.Sprintf("msgs=%d roles=[%s] parts=[%s]", len(msgs), strings.Join(roles, ","), strings.Join(parts, " "))
}

// imageURLShape 描述一个 image_url.url 的形状（不落正文）。
func imageURLShape(u string) string {
	if u == "" {
		return "image_url{空url}"
	}
	if strings.HasPrefix(u, "data:") {
		head, b64, _ := strings.Cut(u, ",")
		return fmt.Sprintf("image_url{%s len=%d}", strings.TrimPrefix(head, "data:"), len(b64))
	}
	if len(u) > 80 {
		u = u[:80] + "…"
	}
	return "image_url{" + u + "}"
}

// teleagentNormalizeMessages 清洗上游不认的消息形态。
//
// 2026-11-03 实测（DSH 走 TeleAgent 时踩到）：
//   - role "developer" → 上游 400 「角色信息不正确」。OpenAI 新把 system 叫 developer，
//     TeleAgent 只认 system/user/assistant/tool，这里统一降级成 system。
//   - image_url.url 不是 data:/http(s): 开头（例如附件 URI、相对路径、空串）→ 上游整条
//     请求 400 「content.type 参数非法」。这种 part 上游要不了，直接丢掉并留痕。
func teleagentNormalizeMessages(payload map[string]any) {
	msgs, _ := payload["messages"].([]any)
	dropped := 0
	rolesFixed := 0
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "developer" {
			m["role"] = "system"
			rolesFixed++
		}
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			t, _ := pm["type"].(string)
			if t == "image_url" || t == "input_image" {
				// 2026-11-03 实测：DSH 发来的图片 part 有两种写法 ——
				//   {"type":"image_url","image_url":{"url":"data:…"}}   ← 标准 OpenAI
				//   {"type":"image_url","image_url":"data:…"}            ← 字符串直给
				// 后一种上游直接 400「content.type 参数非法」，这里统一成对象形态。
				var u string
				switch v := pm["image_url"].(type) {
				case string:
					u = v
				case map[string]any:
					u, _ = v["url"].(string)
				}
				if t == "input_image" {
					pm["type"] = "image_url"
				}
				if u == "" || !(strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
					dropped++ // 上游用不了的图片引用
					continue
				}
				pm["image_url"] = map[string]any{"url": u}
				pm["type"] = "image_url"
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			// 整条消息都被丢掉时补一句占位，避免上游抱怨空 content
			kept = append(kept, map[string]any{"type": "text", "text": "[图片无法转交给该模型]"})
		}
		m["content"] = kept
	}
	if rolesFixed > 0 || dropped > 0 {
		logHub("teleagent 消息清洗：role developer→system %d 条，丢弃无法转交的图片 %d 个", rolesFixed, dropped)
	}
}

// hasImageParts 请求里是否含图片 part。
func hasImageParts(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	msgs, _ := payload["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		parts, _ := m["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if t, _ := pm["type"].(string); t == "image_url" {
				return true
			}
		}
	}
	return false
}

// stripImageParts 去掉所有图片 part，返回去掉后的请求体（没有图片可去时 ok=false）。
func stripImageParts(payload map[string]any) ([]byte, bool) {
	if payload == nil {
		return nil, false
	}
	msgs, _ := payload["messages"].([]any)
	changed := false
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if t, _ := pm["type"].(string); t == "image_url" {
				changed = true
				continue
			}
			kept = append(kept, p)
		}
		if len(kept) == 0 {
			kept = append(kept, map[string]any{"type": "text", "text": "[图片未能转交给该模型]"})
		}
		m["content"] = kept
	}
	if !changed {
		return nil, false
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return b, true
}

// teleagentPartsSample 采样前 n 个图片 part，输出「键名 + 值的形态」，
// 不含 base64 正文（只给长度）。用于定位上游为什么拒收。
func teleagentPartsSample(payload map[string]any, n int) []string {
	if payload == nil {
		return nil
	}
	out := make([]string, 0, n)
	msgs, _ := payload["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		parts, _ := m["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if t, _ := pm["type"].(string); t != "image_url" {
				continue
			}
			keys := make([]string, 0, len(pm))
			for k := range pm {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var urlShape string
			switch v := pm["image_url"].(type) {
			case string:
				urlShape = "image_url=string(" + imageURLShape(v) + ")"
			case map[string]any:
				ik := make([]string, 0, len(v))
				for k := range v {
					ik = append(ik, k)
				}
				sort.Strings(ik)
				u, _ := v["url"].(string)
				urlShape = fmt.Sprintf("image_url.keys=%v url=%s", ik, imageURLShape(u))
			default:
				urlShape = fmt.Sprintf("image_url=%T", v)
			}
			role, _ := m["role"].(string)
			out = append(out, fmt.Sprintf("role=%s keys=%v %s", role, keys, urlShape))
			if len(out) >= n {
				return out
			}
		}
	}
	return out
}

// teleagentMaxTokens 读请求里的输出上限（客户端可能用 max_tokens 或 max_completion_tokens）。
func teleagentMaxTokens(payload map[string]any) int {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		switch v := payload[key].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

// teleagentMinTokens 输出预算下限，默认 1024（见上面的实测注释）。
func teleagentMinTokens() int {
	if v := strings.TrimSpace(os.Getenv("AIHUB_TELEAGENT_MIN_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1024
}
