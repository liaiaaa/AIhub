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
var teleagentModels = []struct {
	ID       string
	Display  string
	Context  int
	MaxOut   int
	Disabled bool
}{
	// 名字/上限取自应用自己的 model-catalog.json（前台只展示前三个）；
	// 注意 TeleAgent.jsonc 的 provider.limits 写的是 1,000,000/65,536，
	// 与目录的 400,000 不一致 —— 这里取较小的 400,000（宁少填，免得客户端超发）。
	{ID: "chat-pro", Display: "智能", Context: 400000, MaxOut: 65536},
	{ID: "chat-flagship", Display: "旗舰", Context: 400000, MaxOut: 65536},
	{ID: "chat-flash", Display: "极速", Context: 400000, MaxOut: 65536},
	// 轻量/轻速：配置里有、前台不展示，但实测可调用（2026-11-02 用 chat-lite/chat-nano 各打一次均 200）
	{ID: "chat-lite", Display: "轻量", Context: 100000, MaxOut: 16384},
	{ID: "chat-nano", Display: "轻速", Context: 100000, MaxOut: 16384},
	// 应用自己把它放在 blacklist 里，不对外暴露
	{ID: "tele-safety-answer", Display: "安全审核", Disabled: true},
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
	p.cli = &http.Client{Transport: directClient().Transport, Timeout: 0}
	p.ln = ln
	p.srv = newLineServer(p)
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
	p.log.Log("==== proxy 启动（%d，模型 %d 个）====", p.Port, len(teleagentModels))
	return true, "已启动（TeleAgent · " + fmt.Sprint(len(teleagentModels)-1) + " 个模型）"
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
		if m.Disabled {
			continue
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
	if backend != probe.Model {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			m["model"] = backend
			if b, err := json.Marshal(m); err == nil {
				body = b
			}
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

	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
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
		cli = &http.Client{Transport: directClient().Transport, Timeout: 0}
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
