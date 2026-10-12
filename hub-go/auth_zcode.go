package main

// Zcode 凭据：自动解密桌面端/CLI 的凭据文件，零配置接入（对齐 Qoder 的体验）。
//
// 来源与格式（2026-09-26 从桌面端 resources/glm/zcode.cjs 逆向确认）：
//   - 文件：%USERPROFILE%\.zcode\v2\credentials.json（桌面端 ZCode.exe 与 CLI 共享）
//   - 信封：`enc:v1:` + base64url(iv[12]) + "." + base64url(tag[16]) + "." +
//     base64url(ciphertext)，AES-256-GCM
//   - 密钥：sha256(secret)；secret = 环境变量 ZCODE_CREDENTIAL_SECRET（优先），
//     否则回退串 `zcode-credential-fallback:{platform}:{homedir}:{username}`
//     （platform 为 Node os.platform() 形态：windows→"win32"）
//   - 取用条目：键名含 ":coding-plan:" 且以 ":api-key" 结尾的条目，值即
//     Coding Plan API Key（`{id}.{secret}` 形态），直接作 Bearer 调
//     https://api.z.ai/api/coding/paas/v4/chat/completions
//
// 桌面端重新登录后文件会更新，401 时重读即可（Reload）。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// zcodeCredentialsPath 凭据文件路径（可用 ZCODE_CREDENTIALS_PATH 覆盖）。
func zcodeCredentialsPath() string {
	if p := os.Getenv("ZCODE_CREDENTIALS_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".zcode/v2/credentials.json"
	}
	return filepath.Join(home, ".zcode", "v2", "credentials.json")
}

// zcodeNodePlatform 对齐 Node os.platform() 的取值。
func zcodeNodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}

// zcodeCurrentUserName 取当前用户名并剥离域前缀（对齐 Node os.userInfo().username）。
func zcodeCurrentUserName() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	} else if envName := os.Getenv("USERNAME"); envName != "" {
		name = envName
	}
	if idx := strings.LastIndex(name, "\\"); idx >= 0 {
		name = name[idx+1:]
	}
	return name
}

// zcodeCipherKey 派生 AES-256-GCM 密钥。
func zcodeCipherKey() []byte {
	secret := strings.TrimSpace(os.Getenv("ZCODE_CREDENTIAL_SECRET"))
	if secret == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "?"
		}
		secret = fmt.Sprintf("zcode-credential-fallback:%s:%s:%s",
			zcodeNodePlatform(), home, zcodeCurrentUserName())
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// zcodeDecrypt 解开 enc:v1 信封；非 enc:v1 值原样返回。
func zcodeDecrypt(key []byte, v string) (string, error) {
	const prefix = "enc:v1:"
	if !strings.HasPrefix(v, prefix) {
		return v, nil
	}
	parts := strings.Split(v[len(prefix):], ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid ciphertext format")
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(iv) != 12 {
		return "", fmt.Errorf("invalid IV")
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(tag) != 16 {
		return "", fmt.Errorf("invalid auth tag")
	}
	ct, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("invalid ciphertext")
	}
	cb, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(cb)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("key mismatch or corrupted ciphertext")
	}
	return string(plain), nil
}

// ZcodeAuth 桌面端凭据持有者：Coding Plan API Key + 账号摘要。
type ZcodeAuth struct {
	mu           sync.RWMutex
	credsPath    string
	apiKey       string // Coding Plan API Key（id.secret）
	jwt          string // zcodejwttoken（start-plan 中继身份，billing 查询用）
	accessToken  string // oauth:zai:access_token（诊断用，不外发）
	userID       string
	userEmail    string
	userName     string
	activeProver string
}

// NewZcodeAuth 读取并解密桌面端凭据。
func NewZcodeAuth() (*ZcodeAuth, error) {
	a := &ZcodeAuth{credsPath: zcodeCredentialsPath()}
	if err := a.Reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload 重读并解密凭据文件（启动与 401 自愈时调用）。
func (a *ZcodeAuth) Reload() error {
	raw, err := os.ReadFile(a.credsPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败: %w", a.credsPath, err)
	}
	var creds map[string]string
	if err := json.Unmarshal(raw, &creds); err != nil {
		return fmt.Errorf("解析凭据 JSON 失败: %w", err)
	}
	key := zcodeCipherKey()

	plain := make(map[string]string, len(creds))
	for k, v := range creds {
		p, derr := zcodeDecrypt(key, v)
		if derr != nil {
			continue // 单条解密失败不影响其他条目
		}
		plain[k] = p
	}

	// Coding Plan API Key：键名含 :coding-plan: 且以 :api-key 结尾。
	// 候选有两族（官方内置规则原文）：zai-individual-coding-plan（API Key
	// 直连计费）与 zai-start-plan（订阅中继身份）。Go map 遍历无序，必须
	// 确定性选择：优先 individual-coding-plan，否则取首个。
	apiKey := ""
	for k, v := range plain {
		if !strings.Contains(k, ":coding-plan:") || !strings.HasSuffix(k, ":api-key") {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.Contains(k, "individual-coding-plan") {
			apiKey = v
			break
		}
		if apiKey == "" {
			apiKey = v
		}
	}
	if apiKey == "" {
		return fmt.Errorf("凭据中无 Coding Plan API Key，请先在 Zcode 桌面端登录")
	}

	a.mu.Lock()
	a.apiKey = apiKey
	a.jwt = strings.TrimSpace(plain["zcodejwttoken"])
	a.accessToken = plain["oauth:zai:access_token"]
	a.activeProver = plain["oauth:active_provider"]
	// user_info: {"user_id": "...", "email": "...", ...}
	var ui struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Name   string `json:"name"`
	}
	if raw := plain["oauth:zai:user_info"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &ui)
	}
	a.userID = ui.UserID
	a.userEmail = ui.Email
	a.userName = ui.Name
	a.mu.Unlock()
	return nil
}

// Key 返回当前 Coding Plan API Key。
func (a *ZcodeAuth) Key() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.apiKey
}

// JWT 返回 zcodejwttoken（start-plan 中继身份；billing 查询用，不外发日志）。
func (a *ZcodeAuth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.jwt
}

// Summary 返回给 health/控制台用的账号摘要（脱敏）。
func (a *ZcodeAuth) Summary() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	head := a.apiKey
	if len(head) > 10 {
		head = head[:6] + "..." + head[len(head)-4:]
	}
	return map[string]any{
		"type":            "zcode_desktop_credential",
		"key_head":        head,
		"user_id":         a.userID,
		"user_email":      a.userEmail,
		"user_name":       a.userName,
		"active_provider": a.activeProver,
		"creds_path":      a.credsPath,
	}
}

// SaveAPIKey 保存手动 API Key 到数据目录（无桌面端登录时的后备凭据）。
func SaveAPIKey(key string) error {
	keyPath := dataPath("zcode_api_key.txt")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		return fmt.Errorf("创建目录失败：%v", err)
	}
	return os.WriteFile(keyPath, []byte(strings.TrimSpace(key)), 0600)
}

// LoadAPIKeyFromFile 从数据目录读取手动保存的 API Key。
func LoadAPIKeyFromFile() (string, error) {
	b, err := os.ReadFile(dataPath("zcode_api_key.txt"))
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("empty key file")
	}
	return k, nil
}
