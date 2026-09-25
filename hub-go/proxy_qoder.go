package main

// Qoder CN 代理：阿里 Qoder 国内版（桌面端凭据）→ 本地 OpenAI 兼容路由。
//
// 与国内/国际/MiMo 代理的区别：
//   - COSY 签名每个请求都不同（绑定 payload+时间戳+body），套不进 AuthProvider
//     的「静态请求头」模型，因此走独立的 QoderProxy 类型，
//     对 Hub 只暴露同款 Start/Stop/Running。
//   - 凭据零配置：启动时用 DPAPI 解密桌面端 %APPDATA%\com.qodercn.app.stable\
//     的 Local State → auth.v1.dat（Chromium safeStorage 格式，AES-256-GCM），
//     拿到 dt- 设备令牌。桌面端平时会自动刷新该文件，401 时这里重读即可。
//   - machineToken：优先现跑 runtime-info.exe 取新鲜值；失败时回退
//     环境变量 QODER_MACHINE_TOKEN，再回退内置的已验证值。
//   - 上游 SSE 是包装帧 {"headers":...,"body":"<OpenAI chunk JSON>"}，
//     解包后内层就是标准 OpenAI chunk；body=="[DONE]" 即结束。
//   - 瞬时故障自愈：401 重读凭据重签；5xx/网络抖动/上游首帧 provider_error
//     （配额偶发抽风）自动重试一次。不自动换模型，避免静默改变行为。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	QoderPort     = 8797
	QoderInferURL = "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation"

	// QoderDefaultMd 默认模型必须是**内部 key**，不是界面显示名。
	// qfmodel = Qwen3.8-Flash（官方免费档）。历史版本误用显示名 "Qwen3.8-Flash"，
	// 该名字并非合法 key，会依赖网关的静默兜底行为，故修正。
	QoderDefaultMd = "qfmodel"

	// runtime-info.exe 的兜底 machineToken（2026-09-22 实测可用的最后已知值；
	// 正常情况下启动时会现跑 runtime-info.exe 取新鲜值，走不到这里）。
	qoderMachineTokenFallback = "P1gAuQTUR1HKGk0h8gjRW3heL7u8jHBmj4zp_ElDzDXm-Go0ngjtOr4LAyxhktdGncmW8uAT0PyOT664xF-Vnp0y"
)

// Qoder COSY 体系 RSA 公钥（从桌面端 app 包逆向所得，用于加密会话密钥）。
const qoderCosyPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// ---------- 凭据（DPAPI + AES-GCM 自动解密） ----------

var qCrypt32 = syscall.NewLazyDLL("crypt32.dll")
var qProcCryptUnprotectData = qCrypt32.NewProc("CryptUnprotectData")

type qDataBlob struct {
	cbData uint32
	pbData *byte
}

func qCryptUnprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	var in qDataBlob
	in.cbData = uint32(len(data))
	in.pbData = &data[0]
	var out qDataBlob
	r, _, err := qProcCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, err
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(out.pbData)))
	res := make([]byte, out.cbData)
	copy(res, unsafe.Slice(out.pbData, out.cbData))
	return res, nil
}

type qoderUser struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	AvatarURL string `json:"avatarUrl"`
}

type qoderCreds struct {
	SchemaVersion         int       `json:"schemaVersion"`
	Token                 string    `json:"token"`
	RefreshToken          string    `json:"refreshToken"`
	ExpiresAt             string    `json:"expiresAt"`
	RefreshTokenExpiresAt string    `json:"refreshTokenExpiresAt"`
	User                  qoderUser `json:"user"`
}

// QoderAuth 保存解密后的凭据与机器标识，负责 COSY 签名。
type QoderAuth struct {
	mu           sync.RWMutex
	creds        qoderCreds
	machineID    string
	machineToken string
	rsaPK        *rsa.PublicKey
}

func NewQoderAuth() (*QoderAuth, error) {
	block, _ := pem.Decode([]byte(qoderCosyPublicKey))
	if block == nil {
		return nil, fmt.Errorf("解析 COSY RSA 公钥失败")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析公钥结构失败: %w", err)
	}
	pk, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("公钥非 RSA 类型")
	}
	return &QoderAuth{rsaPK: pk}, nil
}

// Reload 重新解密桌面端凭据文件（启动与 401 自愈时调用）。
func (a *QoderAuth) Reload() error {
	base := filepath.Join(qoderAppDataDir())

	// 1. Local State → DPAPI → 主密钥
	lsBytes, err := os.ReadFile(filepath.Join(base, "Local State"))
	if err != nil {
		return fmt.Errorf("读取 Local State 失败: %w", err)
	}
	var ls struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(lsBytes, &ls); err != nil {
		return fmt.Errorf("解析 Local State 失败: %w", err)
	}
	encKey, err := base64.StdEncoding.DecodeString(ls.OsCrypt.EncryptedKey)
	if err != nil || len(encKey) < 5 {
		return fmt.Errorf("无效的 encrypted_key")
	}
	aesKey, err := qCryptUnprotect(encKey[5:])
	if err != nil {
		return fmt.Errorf("DPAPI 解密主密钥失败: %w", err)
	}

	// 2. auth.v1.dat（Chromium safeStorage：v10 + 12B nonce + GCM 密文+tag）
	datBytes, err := os.ReadFile(filepath.Join(base, "auth.v1.dat"))
	if err != nil {
		return fmt.Errorf("读取 auth.v1.dat 失败: %w", err)
	}
	if len(datBytes) < 3+12+16 || string(datBytes[:3]) != "v10" {
		return fmt.Errorf("auth.v1.dat 不是有效的 safeStorage 格式")
	}
	cb, err := aes.NewCipher(aesKey)
	if err != nil {
		return fmt.Errorf("创建 AES Cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(cb)
	if err != nil {
		return fmt.Errorf("创建 GCM 失败: %w", err)
	}
	plain, err := gcm.Open(nil, datBytes[3:15], datBytes[15:], nil)
	if err != nil {
		return fmt.Errorf("AES-GCM 解密 auth.v1.dat 失败: %w", err)
	}
	var creds qoderCreds
	if err := json.Unmarshal(plain, &creds); err != nil {
		return fmt.Errorf("解析凭据 JSON 失败: %w", err)
	}
	if creds.Token == "" {
		return fmt.Errorf("凭据中 token 为空，请先在 Qoder 桌面端登录")
	}

	// 3. machine-id + machineToken
	mid := ""
	if b, err := os.ReadFile(filepath.Join(base, "auth.machine-id")); err == nil {
		mid = strings.TrimSpace(string(b))
	}
	mt := qoderMachineToken()
	if mt == "" {
		mt = qoderMachineTokenFallback
	}

	a.mu.Lock()
	a.creds = creds
	a.machineID = mid
	a.machineToken = mt
	a.mu.Unlock()
	return nil
}

func qoderAppDataDir() string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "com.qodercn.app.stable")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "AppData", "Roaming", "com.qodercn.app.stable")
}

// qoderMachineToken 现跑 runtime-info.exe 取新鲜 machineToken。
// 找不到或超时（8s）返回空串，由调用方回退。
func qoderMachineToken() string {
	exe := qoderRuntimeInfoExe()
	if exe == "" {
		return os.Getenv("QODER_MACHINE_TOKEN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe).Output()
	if err != nil {
		return os.Getenv("QODER_MACHINE_TOKEN")
	}
	var info struct {
		MachineToken string `json:"machineToken"`
	}
	if json.Unmarshal(out, &info) != nil || info.MachineToken == "" {
		return os.Getenv("QODER_MACHINE_TOKEN")
	}
	return info.MachineToken
}

func qoderRuntimeInfoExe() string {
	if env := os.Getenv("QODER_RUNTIME_INFO_EXE"); env != "" && fileExists(env) {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	hits, _ := filepath.Glob(filepath.Join(home, ".qoder-cn", ".bin", "umid-win32-x64-*", "runtime-info.exe"))
	if len(hits) > 0 {
		return hits[0]
	}
	return ""
}

// Snapshot 返回给控制台/health 用的账号摘要（只读内存，不 IO）。
func (a *QoderAuth) Snapshot() (name, uid, tokenHead, expiresAt string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	th := ""
	if len(a.creds.Token) > 8 {
		th = a.creds.Token[:8] + "..."
	}
	return a.creds.User.Name, a.creds.User.ID, th, a.creds.ExpiresAt
}

func qoderPkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - (len(data) % blockSize)
	pad := bytes.Repeat([]byte{byte(padLen)}, padLen)
	return append(data, pad...)
}

// Sign 为指定 URL + body 生成 COSY 签名请求头（每个请求都要现算）。
func (a *QoderAuth) Sign(urlStr, body string) (map[string]string, error) {
	a.mu.RLock()
	creds, machineID, machineToken := a.creds, a.machineID, a.machineToken
	a.mu.RUnlock()

	// 1. 16 字节会话密钥（8 字节随机数的 hex ASCII）
	rawKey := make([]byte, 8)
	if _, err := rand.Read(rawKey); err != nil {
		return nil, err
	}
	keyBuf := []byte(hex.EncodeToString(rawKey))

	// 2. AES-128-CBC（IV=keyBuf）加密用户信息 → info
	userJSON, err := json.Marshal(map[string]string{
		"uid":                  creds.User.ID,
		"aid":                  "",
		"name":                 creds.User.Name,
		"email":                creds.User.Email,
		"security_oauth_token": creds.Token,
	})
	if err != nil {
		return nil, err
	}
	cb, err := aes.NewCipher(keyBuf)
	if err != nil {
		return nil, err
	}
	padded := qoderPkcs7Pad(userJSON, 16)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(cb, keyBuf).CryptBlocks(enc, padded)
	infoB64 := base64.StdEncoding.EncodeToString(enc)

	// 3. RSA-PKCS1v15 加密会话密钥 → Cosy-Key
	rsaEnc, err := rsa.EncryptPKCS1v15(rand.Reader, a.rsaPK, keyBuf)
	if err != nil {
		return nil, err
	}
	keyB64 := base64.StdEncoding.EncodeToString(rsaEnc)

	// 4. payload → 标准 base64
	ridBytes := make([]byte, 16)
	rand.Read(ridBytes)
	requestID := hex.EncodeToString(ridBytes)
	payloadBytes, _ := json.Marshal(map[string]string{
		"version":     "v1",
		"requestId":   requestID,
		"info":        infoB64,
		"cosyVersion": "1.0.0",
		"ideVersion":  "0.3.4",
	})
	payloadB64 := base64.StdEncoding.EncodeToString(payloadBytes)

	// 5. 规范化 path（去 query；/algo 前缀剥离）
	path := urlStr
	if idx := strings.Index(urlStr, "://"); idx != -1 {
		rest := urlStr[idx+3:]
		if slashIdx := strings.Index(rest, "/"); slashIdx != -1 {
			path = rest[slashIdx:]
		}
	}
	if qIdx := strings.Index(path, "?"); qIdx != -1 {
		path = path[:qIdx]
	}
	if strings.HasPrefix(path, "/algo") {
		path = path[5:]
		if path == "" {
			path = "/"
		}
	}

	// 6. MD5 链签名
	ts := time.Now().Unix()
	signStr := fmt.Sprintf("%s\n%s\n%d\n%s\n%s", payloadB64, keyB64, ts, body, path)
	hash := md5.Sum([]byte(signStr))

	headers := map[string]string{
		"Accept":          "text/event-stream, application/json",
		"Content-Type":    "application/json",
		"Authorization":   fmt.Sprintf("Bearer COSY.%s.%s", payloadB64, hex.EncodeToString(hash[:])),
		"Cosy-User":       creds.User.ID,
		"Cosy-Key":        keyB64,
		"Cosy-Date":       fmt.Sprintf("%d", ts),
		"Cosy-ClientType": "0",
		"Cosy-Version":    "1.0.0",
		"X-Request-Id":    requestID,
		"X-IDE-Platform":  "Qoder",
		"X-Version":       "0.3.4",
	}
	if machineID != "" {
		headers["Cosy-MachineId"] = machineID
	}
	if machineToken != "" {
		headers["Cosy-MachineToken"] = machineToken
	}
	return headers, nil
}

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
	}
	t0 := time.Now()

	if clientStream {
		p.streamToClient(w, r, payload, t0, rid)
		return
	}
	p.aggregateToClient(w, r, payload, modelName, t0, rid)
}

// ---------- 思考控制 ----------
//
// 背景（2026-09-24 破解定论，来源：官方 worker runtime 源码 + proto/chat.proto
// + 官方模型目录 thinking_config 三方交叉验证）：
//  1. Qoder 网关**有**真实思考强度控制，字段在请求体 `parameters` 对象内
//     （官方 runtime 的请求组装函数把它放在 parameters，而不是顶层）：
//       parameters.reasoning_effort        = none|low|medium|high|xhigh|max
//       parameters.enable_thinking         = true|false
//       parameters.reasoning_budget_tokens = N（另有别名 thinking_budget）
//       parameters.preserve_thinking       = true|false
//     响应侧 thinking 以 delta.reasoning_content 流回（proto ResponseChatMessage）。
//  2. 每个模型支持的强度档与默认档来自官方目录 thinking_config.efforts
//     （如 qfmodel：low/medium/xhigh，默认 medium；gmodel：low/high/max，默认 max）。
//     官方客户端**每个请求都发送** parameters.reasoning_effort（未设置时取目录默认档）。
//  3. 历史教训：此前的“字段全部无效”结论是**无效对照**得出的——所有 A/B 实验恰好
//     都测在模型默认值上（gmodel 默认即 max），或把字段放在顶层/写成驼峰
//     （parameters.reasoningEffort），因此看不出差异。
//
// 本节把客户端的思考意图（reasoning_effort / thinking / enable_thinking /
// thinking_budget / 消息内标记）翻译成上述 parameters 字段，默认取目录默认档
// （= 默认真思考）。旧的“注入强制 <think> 格式系统提示词”方案已废弃：它不产生真思考，
// 还扭曲输出格式，是“干什么活都干杂”的元凶。

const (
	// 消息内关闭标记（对任何客户端都可用的按次开关）。
	qoderNoThinkMarker    = "[no-thinking]"
	qoderNoThinkMarkerAlt = "[nothink]"
)

// qoderEffortOrder 强度规范序（与官方 runtime 一致）。
var qoderEffortOrder = []string{"none", "low", "medium", "high", "xhigh", "max"}

// qoderNormalizeEffort 把各种叫法归一为官方强度档；无法识别返回空串。
func qoderNormalizeEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "disabled", "off", "disable":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium", "moderate":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "ultra":
		return "xhigh"
	case "max", "maximum":
		return "max"
	}
	return ""
}

// qoderEffortForBudget 按官方 kZ 换算：预算 token 数 → 强度档。
func qoderEffortForBudget(n int) string {
	switch {
	case n <= 0:
		return "none"
	case n <= 1024:
		return "low"
	case n <= 8192:
		return "medium"
	case n <= 24576:
		return "high"
	case n <= 49152:
		return "xhigh"
	default:
		return "max"
	}
}

// qoderIntVal 从 json.Number / float64 / int 等取整数值。
func qoderIntVal(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return int(i), true
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// qoderThinkingSpec 客户端思考意图的归一化结果。
type qoderThinkingSpec struct {
	Effort   string // none|low|medium|high|xhigh|max；空 = 不发送
	Enabled  *bool  // enable_thinking；nil = 不发送
	Budget   int    // reasoning_budget_tokens；0 = 不发送
	Preserve *bool  // preserve_thinking；nil = 不发送
}

// qoderResolveThinking 解析客户端意图（优先级：消息内标记 > 请求体显式字段 >
// 目录默认档），并做两步规范化：
//   - budget → 强度档换算（未显式给档位时）
//   - 档位不在该模型支持列表时回退目录默认档（绝不发非法值）
func qoderResolveThinking(body map[string]any, entry *qoderModelEntry) qoderThinkingSpec {
	spec := qoderThinkingSpec{}
	explicit := false

	// 1) 显式强度字段（OpenAI 风格 reasoning_effort / 简写 effort）
	for _, k := range []string{"reasoning_effort", "effort"} {
		if s, ok := body[k].(string); ok {
			if e := qoderNormalizeEffort(s); e != "" {
				spec.Effort = e
				explicit = true
				break
			}
		}
	}

	// 2) thinking:{type,budget_tokens}（DeepSeek 风格）
	if th, ok := body["thinking"].(map[string]any); ok {
		explicit = true
		switch strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", th["type"]))) {
		case "disabled", "off", "none":
			spec.Effort = "none"
		case "enabled":
			t := true
			spec.Enabled = &t
		}
		if b, ok := qoderIntVal(th["budget_tokens"]); ok {
			spec.Budget = b
		}
	}

	// 3) enable_thinking / budget / preserve
	if v, ok := body["enable_thinking"].(bool); ok {
		explicit = true
		spec.Enabled = &v
	}
	if b, ok := qoderIntVal(body["reasoning_budget_tokens"]); ok {
		explicit = true
		spec.Budget = b
	} else if b, ok := qoderIntVal(body["thinking_budget"]); ok {
		explicit = true
		spec.Budget = b
	}
	if v, ok := body["preserve_thinking"].(bool); ok {
		spec.Preserve = &v
	}

	// budget → 档位换算（仅在未显式指定档位时）
	if spec.Budget > 0 && spec.Effort == "" {
		spec.Effort = qoderEffortForBudget(spec.Budget)
	}
	// 显式关闭压倒一切
	if spec.Enabled != nil && !*spec.Enabled {
		spec.Effort = "none"
	}

	// 默认：该模型支持的最高档（2026-09-24 用户选择：默认最强思考）
	if !explicit && spec.Effort == "" && entry != nil {
		spec.Effort = qoderHighestEffort(entry.Efforts, entry.DefaultEffort)
	}

	// 不支持的档位就近钳制（|距离|最小；同距取更高档），绝不发非法值
	if spec.Effort != "" && spec.Effort != "none" && entry != nil && len(entry.Efforts) > 0 {
		ok := false
		for _, e := range entry.Efforts {
			if e == spec.Effort {
				ok = true
				break
			}
		}
		if !ok {
			spec.Effort = qoderClampEffort(spec.Effort, entry)
		}
	}
	return spec
}

// qoderHighestEffort 取支持档位中的最高档；无档位信息时回退 defaultEffort。
func qoderHighestEffort(efforts []string, defaultEffort string) string {
	best := ""
	for _, e := range efforts {
		if qoderEffortRank(e) > qoderEffortRank(best) {
			best = e
		}
	}
	if best == "" {
		return defaultEffort
	}
	return best
}

// qoderClampEffort 就近钳制：与所请求档位 |距离| 最小的支持档；同距取更高档。
func qoderClampEffort(effort string, entry *qoderModelEntry) string {
	best := ""
	bestDist := 1 << 30
	for _, e := range entry.Efforts {
		d := qoderEffortRank(effort) - qoderEffortRank(e)
		if d < 0 {
			d = -d
		}
		if d < bestDist || (d == bestDist && qoderEffortRank(e) > qoderEffortRank(best)) {
			best, bestDist = e, d
		}
	}
	if best == "" {
		return entry.DefaultEffort
	}
	return best
}

// qoderEffortRank 强度序号（none=0 … max=5）；未知档位 = -1。
func qoderEffortRank(e string) int {
	for i, name := range qoderEffortOrder {
		if name == e {
			return i
		}
	}
	return -1
}

// qoderApplyThinking 按用户意图把思考参数写进 body["parameters"]（官方 wire 形态），
// 并把已翻译的顶层控制字段摘掉（防双重语义）。除注入温和可见思考提示外不改动 messages。
func qoderApplyThinking(body map[string]any, entry *qoderModelEntry) {
	// 消息内标记 = 本次明确不要思考（最具体，优先级最高）
	marked := qoderNoThinkMarked(body)
	qoderStripNoThinkMarker(body)
	spec := qoderThinkingSpec{Effort: "none"}
	if !marked {
		spec = qoderResolveThinking(body, entry)
	}

	params := map[string]any{}
	if p, ok := body["parameters"].(map[string]any); ok {
		for k, v := range p {
			params[k] = v
		}
	}
	switch {
	case spec.Effort == "none":
		params["reasoning_effort"] = "none"
		params["enable_thinking"] = false
		delete(params, "reasoning_budget_tokens")
	case spec.Effort != "":
		params["reasoning_effort"] = spec.Effort
		params["enable_thinking"] = true
		if spec.Budget > 0 {
			params["reasoning_budget_tokens"] = spec.Budget
		}
	case spec.Enabled != nil:
		params["enable_thinking"] = *spec.Enabled
		if spec.Budget > 0 {
			params["reasoning_budget_tokens"] = spec.Budget
		}
	case spec.Budget > 0:
		params["reasoning_budget_tokens"] = spec.Budget
	}
	if spec.Preserve != nil {
		params["preserve_thinking"] = *spec.Preserve
	}
	if len(params) > 0 {
		body["parameters"] = params
	}
	// 顶层控制字段已翻译进 parameters，摘掉防双重语义/上游噪音
	for _, k := range []string{
		"reasoning_effort", "effort", "thinking", "enable_thinking",
		"thinking_budget", "reasoning_budget_tokens", "preserve_thinking",
	} {
		delete(body, k)
	}

	// 可见思考提示：此网关的隐藏思考在裸 HTTP 路径上不可验证（2026-09-24 实测
	// parameters 各形态均无可观测差异），可见推理的可靠杠杆仍是提示词层。
	// 因此默认注入**温和版**（qoderThinkingPrompt），但：
	//   - 显式关闭 / [no-thinking] 标记 → 不注入；
	//   - 带 tools 的 Agent 请求同样注入（用户拍板 2026-09-24），由提示词保护工具调用格式；
	//   - 环境变量 QODER_THINK_PROMPT=off → 全局关闭。
	off := spec.Effort == "none" || (spec.Enabled != nil && !*spec.Enabled)
	if off || qoderThinkPromptDisabled() {
		return
	}
	qoderInjectPrompt(body, qoderThinkingPrompt)
}

// qoderThinkDiag 思考链路一行诊断：把客户端意图→翻译→注入全过程可视化，
// 用于定位"思考框短/没真思考"断在客户端意图、目录档位还是参数注入哪一环。
func qoderThinkDiag(raw, body map[string]any, entry *qoderModelEntry, marked bool) string {
	var b strings.Builder
	b.WriteString("意图{")

	// 1) 客户端显式意图（原始请求体，翻译前）
	first := true
	writeKV := func(k string, v any) {
		if !first {
			b.WriteString(" ")
		}
		first = false
		fmt.Fprintf(&b, "%s=%v", k, v)
	}
	for _, k := range []string{"reasoning_effort", "effort", "enable_thinking",
		"reasoning_budget_tokens", "thinking_budget", "preserve_thinking"} {
		if v, ok := raw[k]; ok {
			writeKV(k, v)
		}
	}
	if th, ok := raw["thinking"].(map[string]any); ok {
		writeKV("thinking", th)
	}
	if marked {
		writeKV("[no-thinking]", true)
	}
	if first {
		b.WriteString("无显式字段(取目录默认档)")
	}
	b.WriteString("}")

	// 2) 目录条目（决定默认档与合法档位）
	if entry == nil {
		b.WriteString(" 目录{未命中}")
	} else {
		fmt.Fprintf(&b, " 目录{efforts=%v default=%s disabled=%v}",
			entry.Efforts, entry.DefaultEffort, entry.SupportsDisabled)
	}

	// 3) 实际注入的 parameters（翻译结果，真正发给上游的思考旋钮）
	fmt.Fprintf(&b, " → parameters{%s}", qoderParamsDiag(body))
	return b.String()
}

// qoderParamsDiag 渲染 body["parameters"] 里与思考相关的字段。
func qoderParamsDiag(body map[string]any) string {
	p, ok := body["parameters"].(map[string]any)
	if !ok {
		return "无 parameters 对象"
	}
	var parts []string
	for _, k := range []string{"reasoning_effort", "enable_thinking",
		"reasoning_budget_tokens", "preserve_thinking"} {
		if v, ok := p[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) == 0 {
		return "思考字段全缺"
	}
	return strings.Join(parts, " ")
}

// qoderThinkingPrompt 可见思考指令：推理写在 <think> 内（代理转写成
// reasoning_content，客户端渲染为折叠思考框），同时明确保护最终输出格式。
// 措辞平衡（2026-09-24 实测教训）：必须强制写思考标签（温和措辞实测 3/3 被
// 模型忽略，可见思考失效）；但不约束答案结构（旧版刚性两段式措辞会扭曲
// 一切输出格式，干活必乱），只保护用户要求的格式。
const qoderThinkingPrompt = "除了一两个词就能回答的极简问题，回答前必须先把完整的推理过程写在 <think> 和" +
	qoderThinkClose + " 之间：明确关键假设、拆解问题、逐项推演并自我检验结论，" + qoderThinkClose +
	" 之后再给最终答案，最终答案只输出用户需要的内容，保持用户要求的格式" +
	"（代码、清单、工具调用 JSON 等原样保留），不要把思考内容混入其中；极简问题可以直接作答。"

// qoderThinkClose 闭合思考标签（拼接构造，规避编辑链路对标签字面量的截断）。
const qoderThinkClose = "</think>"

// qoderThinkPromptDisabled 环境变量总开关（QODER_THINK_PROMPT=off）。
func qoderThinkPromptDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("QODER_THINK_PROMPT")))
	return v == "off" || v == "0" || v == "false"
}

// qoderInjectPrompt 把提示词并入第一个 system 消息（没有则前置一条）。
func qoderInjectPrompt(body map[string]any, prompt string) {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "system" {
			switch c := mm["content"].(type) {
			case string:
				mm["content"] = c + "\n\n" + prompt
			case []any: // 视觉消息等多段 content
				mm["content"] = append(c, map[string]any{"type": "text", "text": prompt})
			default:
				mm["content"] = prompt
			}
			return
		}
	}
	sys := map[string]any{"role": "system", "content": prompt}
	body["messages"] = append([]any{sys}, msgs...)
}

// entryFor 按真实 key 查目录条目（未收录返回 nil）。
func (p *QoderProxy) entryFor(key string) *qoderModelEntry {
	cat := p.catalogSnapshot()
	for i := range cat {
		if cat[i].Key == key {
			return &cat[i]
		}
	}
	return nil
}

// qoderNoThinkMarked 判断最后一条用户消息是否带关闭标记。
func qoderNoThinkMarked(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r != "user" {
			continue
		}
		if c, ok := mm["content"].(string); ok {
			return strings.Contains(c, qoderNoThinkMarker) || strings.Contains(c, qoderNoThinkMarkerAlt)
		}
		return false
	}
	return false
}

// qoderStripNoThinkMarker 从最后一条用户消息里摘除关闭标记（不透传给上游）。
func qoderStripNoThinkMarker(body map[string]any) {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r != "user" {
			continue
		}
		if c, ok := mm["content"].(string); ok {
			if strings.Contains(c, qoderNoThinkMarker) || strings.Contains(c, qoderNoThinkMarkerAlt) {
				c = strings.ReplaceAll(c, qoderNoThinkMarker, "")
				c = strings.ReplaceAll(c, qoderNoThinkMarkerAlt, "")
				mm["content"] = strings.TrimSpace(c)
			}
		}
		return // 只看最后一条用户消息
	}
}

// ---------- 思考/正文分流（<think> → reasoning_content） ----------

// qoderThinkState 流式分流的会话状态（每个请求一个）。
type qoderThinkState struct {
	inThink bool
	buf     string // 跨帧缓冲：可能一个 chunk 里只有 "<thi"，要等下一帧才能判定
}

// qoderMaxTagLen 标签最大长度（"<think>"/"</think>" 均 9 字节内）。
const qoderMaxTagLen = 16

func newQoderThinkState() *qoderThinkState { return &qoderThinkState{} }

// split 把一帧的 content 文本拆成 (正文部分, 思考部分)。
// 标签可能跨帧：不够长无法判定的尾部进 buf，等下一帧。
// 流结束时调用 flush() 吐出残留缓冲。
func (s *qoderThinkState) split(text string) (out, think string) {
	s.buf += text
	for {
		if s.inThink {
			end := strings.Index(s.buf, "</think>")
			if end < 0 {
				// 可能 "</think" 被截断：保留可能构成尾标签的尾部
				keep := qoderSafeKeep(s.buf, "</think>")
				thinkOut := s.buf[:len(s.buf)-keep]
				s.buf = s.buf[len(s.buf)-keep:]
				return out, thinkOut
			}
			think += s.buf[:end]
			s.buf = s.buf[end+len("</think>"):]
			s.inThink = false
			continue
		}
		start := strings.Index(s.buf, "<think>")
		if start < 0 {
			// 可能 "<think" 被截断：保留可能构成头标签的尾部
			keep := qoderSafeKeep(s.buf, "<think>")
			outPart := s.buf[:len(s.buf)-keep]
			s.buf = s.buf[len(s.buf)-keep:]
			return out + outPart, think
		}
		out += s.buf[:start]
		s.buf = s.buf[start+len("<think>"):]
		s.inThink = true
	}
}

// qoderSafeKeep 返回 buf 尾部可能构成 tag 前缀的最长长度。
func qoderSafeKeep(buf, tag string) int {
	max := len(tag) - 1
	if max > len(buf) {
		max = len(buf)
	}
	for k := max; k > 0; k-- {
		if strings.HasPrefix(tag, buf[len(buf)-k:]) {
			return k
		}
	}
	return 0
}

// flush 流结束时吐出残留缓冲（未闭合的 <think> 内容归入思考）。
func (s *qoderThinkState) flush() (out, think string) {
	rest := s.buf
	s.buf = ""
	if s.inThink {
		return "", rest
	}
	return rest, ""
}

// qoderRewriteChunk 对单个内层 OpenAI chunk 做 think→reasoning_content 转写。
// 返回转写后的 JSON 文本；若该帧无变化则返回原文本。
func qoderRewriteChunk(raw string, st *qoderThinkState, final bool) string {
	var chunk map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&chunk) != nil {
		return raw
	}
	changed := false
	for _, ch := range asSlice(chunk["choices"]) {
		cm, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := cm["delta"].(map[string]any)
		if !ok {
			continue
		}
		ct, ok := delta["content"].(string)
		if !ok {
			continue
		}
		var out, think string
		if final {
			// 收尾：把缓冲全部吐出
			out, think = st.split(ct)
			o2, t2 := st.flush()
			out += o2
			think += t2
		} else {
			out, think = st.split(ct)
		}
		// 关键：即使 out/think 都为空，只要输入非空就说明本帧内容被状态机
		// 消费（如整帧恰好是 "<think>" 标签），必须重写帧清空 content，
		// 否则标签原样透传，客户端会把标签当正文显示。
		if ct != "" {
			changed = true
		}
		// reasoning_content 追加（上游本无此字段；一帧可能同时含两者）
		if think != "" {
			prev, _ := delta["reasoning_content"].(string)
			delta["reasoning_content"] = prev + think
			changed = true
		}
		if out != "" {
			delta["content"] = out
		} else {
			delete(delta, "content") // 该帧纯标签/纯思考：正文置空
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return raw
	}
	return string(b)
}

// qoderRewriteStreamForThinking 对整段内层 SSE 做逐帧 think→reasoning_content 转写
// （非流式聚合路径用）。
func qoderRewriteStreamForThinking(sse string) string {
	st := newQoderThinkState()
	var out strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") || trimmed == "data: [DONE]" {
			out.WriteString(line)
			out.WriteString("\n")
			continue
		}
		raw := strings.TrimSpace(trimmed[5:])
		out.WriteString("data: " + qoderRewriteChunk(raw, st, false) + "\n")
	}
	// 残留缓冲并入最后一个 content 帧
	if rest, think := st.flush(); rest != "" || think != "" {
		out.WriteString("data: " + jsonMust(map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": rest, "reasoning_content": think},
		}}}) + "\n")
	}
	return out.String()
}

func jsonMust(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// qoderNormalizeMessages：developer → system（对齐其他代理的行为）。
func qoderNormalizeMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		if role, _ := mm["role"].(string); role == "developer" {
			nm := cloneMap(mm)
			nm["role"] = "system"
			out = append(out, nm)
			changed = true
			continue
		}
		out = append(out, m)
	}
	if !changed {
		return msgs
	}
	return out
}

// openInfer 签名并调用上游，带自愈重试：
//   - 网络错误 / 5xx：等 700ms 重试（最多 3 次尝试）
//   - 401：重读桌面端凭据后重签重试（桌面端平时会自动刷新 auth.v1.dat）
//
// 返回的 resp 由调用方关闭。
func (p *QoderProxy) openInfer(ctx context.Context, payload []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(700 * time.Millisecond)
		}
		headers, serr := p.auth.Sign(p.inferURL, string(payload))
		if serr != nil {
			return nil, serr
		}
		req, rerr := http.NewRequestWithContext(ctx, "POST", p.inferURL, bytes.NewReader(payload))
		if rerr != nil {
			return nil, rerr
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err = p.client.Do(req)
		if err != nil {
			if attempt == 2 {
				return nil, err
			}
			continue // 网络抖动重试
		}
		if resp.StatusCode == http.StatusUnauthorized {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			resp = nil
			if rerr := p.auth.Reload(); rerr != nil && attempt == 2 {
				return nil, fmt.Errorf("凭据已过期且重读失败：%v", rerr)
			}
			continue // 换新凭据重试
		}
		if resp.StatusCode >= 500 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			resp = nil
			if attempt == 2 {
				return nil, fmt.Errorf("上游持续 5xx")
			}
			continue
		}
		return resp, nil
	}
	if resp == nil && err == nil {
		err = fmt.Errorf("上游调用失败")
	}
	return resp, err
}

// firstFrameUpstreamError 检查首帧是否为上游错误帧（如配额瞬时抽风的
// provider_error，HTTP 状态仍是 200，只能从内容识别）。
func firstFrameUpstreamError(head []byte) bool {
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		var probe struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Choices []any  `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &probe) != nil {
			return false
		}
		// 上游错误帧：有 code/message、没有 choices；正常帧要么是包装帧（有 body），要么有 choices
		if probe.Code != "" && probe.Message != "" && probe.Choices == nil {
			return true
		}
		return false
	}
	return false
}

// openInferValidated 在 openInfer 之上多做一层首帧错误检测：
// HTTP 200 但首帧是 provider_error（如配额瞬时抽风）时整体重试一次。
// Peek(1) 只等到上游第一个字节到达（通常 ~300ms），不会人为增加首包延迟。
//
// ⚠️ 关键：bufio.Peek **不消费**缓冲区，Peek 过的字节仍留在 br 里——
// 所以这里直接把 br 包回 resp.Body，绝不能把预读的 head 再拼回流前面
// （否则已缓冲字节会被读两遍，客户端 JSON.parse 报
// "Unexpected non-whitespace character after JSON"）。
func (p *QoderProxy) openInferValidated(ctx context.Context, payload []byte, log *FileLogger, rid string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := p.openInfer(ctx, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return resp, nil // 非 200 交给上层报错
		}
		br := bufio.NewReaderSize(resp.Body, 16*1024)
		perr := error(nil)
		if _, perr = br.Peek(1); perr != nil && perr != io.EOF {
			// 首字节都读不出来（网络/上下文异常）：原样交回，上层按流错误处理
			resp.Body = &qBodyReader{r: br, c: resp.Body}
			return resp, nil
		}
		head, _ := br.Peek(br.Buffered()) // 只用于错误帧探测，不消费
		if attempt == 0 && len(head) > 0 && firstFrameUpstreamError(head) {
			resp.Body.Close()
			if log != nil {
				log.Log("[%s] ⚠ 上游首帧 provider_error，700ms 后重试", rid)
			}
			time.Sleep(700 * time.Millisecond)
			continue
		}
		resp.Body = &qBodyReader{r: br, c: resp.Body}
		return resp, nil
	}
}

// qBodyReader 包装重缓冲后的响应流（br 内保有 Peek 过但未消费的字节），
// Close 透传给原始 body。
type qBodyReader struct {
	r io.Reader
	c io.Closer
}

func (b *qBodyReader) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *qBodyReader) Close() error               { return b.c.Close() }

// qoderUnwrapFrames 把上游包装帧流转成内层 OpenAI SSE 文本。
// 返回首帧错误信息（空串表示正常）；聚合模式整体缓冲使用。
func qoderUnwrapFrames(r io.Reader, out *bytes.Buffer) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	firstErr := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[5:])
		var wrapper qoderSSEWrapper
		if json.Unmarshal([]byte(raw), &wrapper) != nil || wrapper.Body == "" {
			// 不是包装帧：可能是裸错误帧，也可能是裸 OpenAI chunk
			var probe map[string]any
			if json.Unmarshal([]byte(raw), &probe) == nil {
				if c, _ := probe["code"].(string); c != "" {
					if _, hasMsg := probe["message"]; hasMsg {
						if _, hasChoices := probe["choices"]; !hasChoices {
							if firstErr == "" {
								firstErr = raw
							}
							continue
						}
					}
				}
			}
			fmt.Fprintf(out, "data: %s\n\n", raw)
			continue
		}
		if wrapper.Body == "[DONE]" {
			out.WriteString("data: [DONE]\n\n")
			continue
		}
		fmt.Fprintf(out, "data: %s\n\n", wrapper.Body)
	}
	return firstErr
}

func (p *QoderProxy) streamToClient(w http.ResponseWriter, r *http.Request, payload []byte, t0 time.Time, rid string) {
	thinkFrames := 0 // 诊断：含 reasoning_content 的帧数
	resp, err := p.openInferValidated(r.Context(), payload, p.log, rid)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		sseError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
		sseError(w, resp.StatusCode, truncate(string(b), 500))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// 边解包边转发：包装帧 → 内层 OpenAI chunk，逐帧 flush（首块已由
	// openInferValidated 拼回 body 最前，不会丢帧）。
	// 思考分流：<think> 标签内容 → reasoning_content（客户端渲染成折叠思考框）。
	st := newQoderThinkState()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "event:") || !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(line[5:])
		var wrapper qoderSSEWrapper
		if json.Unmarshal([]byte(raw), &wrapper) == nil && wrapper.Body != "" {
			raw = wrapper.Body
		}
		if raw == "[DONE]" {
			fmt.Fprintf(w, "data: [DONE]\n\n")
		} else {
			rw := qoderRewriteChunk(raw, st, false)
			if strings.Contains(rw, `"reasoning_content"`) {
				thinkFrames++
			}
			fmt.Fprintf(w, "data: %s\n\n", rw)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 流结束：吐出分流缓冲的残留（缓冲里的内容已无法归帧，直接并入正文）
	if out, think := st.flush(); out != "" || think != "" {
		tail := map[string]any{"choices": []any{map[string]any{
			"index": 0, "finish_reason": nil,
			"delta": func() map[string]any {
				d := map[string]any{}
				if think != "" {
					d["reasoning_content"] = think
				}
				if out != "" {
					d["content"] = out
				}
				return d
			}(),
		}}}
		if b, err := json.Marshal(tail); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | stream | think=%d帧", rid, time.Since(t0).Seconds(), thinkFrames)
	}
}

func (p *QoderProxy) aggregateToClient(w http.ResponseWriter, r *http.Request, payload []byte, model string, t0 time.Time, rid string) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	resp, err := p.openInferValidated(ctx, payload, p.log, rid)
	if err != nil {
		if p.log != nil {
			p.log.Log("[%s] ✗ 网络错误 | %v", rid, err)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "upstream error: " + err.Error(), "type": "upstream_error"},
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if p.log != nil {
			p.log.Log("[%s] ✗ HTTP %d | %s", rid, resp.StatusCode, truncate(string(b), 200))
		}
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

	// 整体解包成内层 SSE，再复用 hub 的聚合器（tool_calls/usage/finish 全支持）。
	// 思考分流：<think> 内容 → reasoning_content（聚合时合并进 message）。
	var inner bytes.Buffer
	if ferr := qoderUnwrapFrames(resp.Body, &inner); ferr != "" {
		// 首帧即上游错误（重试后仍失败）：透传给客户端
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": truncate(ferr, 500), "type": "upstream_error"},
		})
		return
	}
	innerStr := qoderRewriteStreamForThinking(inner.String())
	col := collectStream(strings.NewReader(innerStr))
	if col.model != "" && col.model != "unknown" {
		model = col.model
	}
	elapsed := time.Since(t0)
	tokens := "?"
	if col.usage != nil {
		if v, ok := col.usage["total_tokens"].(json.Number); ok {
			tokens = v.String()
		}
	}
	if p.log != nil {
		p.log.Log("[%s] ◀ RESPONSE | %.1fs | finish=%s | tokens=%s | think=%d段", rid, elapsed.Seconds(), col.finish, tokens, len(col.reasoning))
	}
	res := col.build()
	res["model"] = model
	writeJSON(w, http.StatusOK, res)
}
