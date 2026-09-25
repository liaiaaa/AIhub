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



