package main

// TeleAgent（TeleAI 的 Electron 客户端）凭据读取（2026-11-02）。
//
// 它把上游 token 用 Electron `safeStorage` 加密后落在：
//
//	~/.local/share/TeleAgent/users/<user>/app-auth/token.json
//	{"scheme":"safe_storage","encryptedToken":"<base64>","encryptedMeta":"...","version":...}
//
// 实测该机器上解出来的是 601 字符 JWT，加密形态就是 Chromium/Electron 在 Linux 下
// 那套 OSCrypt（v10/v11 前缀 + AES-128-CBC）：
//
//	password = gnome-keyring 里 application=TeleAgent 的那条 secret（secret-tool 可读）
//	key      = PBKDF2-HMAC-SHA1(password, "saltysalt", 1 轮, 16 字节)
//	plain    = AES-128-CBC(key, IV=16 个空格).Decrypt(ciphertext[3:])
//	明文     = "<id>:<JWT>"（冒号前是应用自己的标识，取冒号后那段）
//
// 于是不需要启动 GUI，也不需要往 keyring 里写东西，纯本地就能拿到 token。
// 注意：这里的「解出用户的 token」只在本机内存里用，绝不落盘、绝不写日志。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	teleagentKeyringApp = "TeleAgent"
	// Chromium/Electron 在 Linux 上的固定盐与循环次数（1 轮）。
	teleagentSalt      = "saltysalt"
	teleagentKDFRounds = 1
	teleagentKeyLen    = 16
)

// teleagentTokenState 缓存解出来的 token（token 是长有效期 JWT，缓存 30 分钟足够，
// 每次请求都 exec 一次 secret-tool 太浪费）。
var teleagentTokenState = struct {
	sync.Mutex
	token   string
	fetched time.Time
}{}

// TeleAgentTokenPath 找当前用户的 token.json：多个 user 目录时取最近修改的那个。
func teleagentTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	pattern := filepath.Join(home, ".local", "share", "TeleAgent", "users", "*", "app-auth", "token.json")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf("未找到 TeleAgent 登录态（%s）", pattern)
	}
	sort.Slice(matches, func(i, j int) bool {
		si, _ := os.Stat(matches[i])
		sj, _ := os.Stat(matches[j])
		if si == nil || sj == nil {
			return matches[i] < matches[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	return matches[0], nil
}

// teleagentSafeStoragePassword 读 gnome-keyring 里 Electron 用的那条密钥。
//
// Electron 在 Linux 把 safeStorage 的派生口令存在 keyring（属性 application=<app 名>），
// 用 secret-tool 读出来的就是那串口令本身（base64 形态的随机串）。
func teleagentSafeStoragePassword() (string, error) {
	cmd := exec.Command("secret-tool", "search", "--all", "application", teleagentKeyringApp)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("读 keyring 失败（应用未登录过或 secret-tool 不可用）：%v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "secret ="); i >= 0 {
			if pw := strings.TrimSpace(line[i+len("secret ="):]); pw != "" {
				return pw, nil
			}
		}
	}
	return "", errors.New("keyring 里没有 application=TeleAgent 的条目")
}

// pbkdf2SHA1Key 仅支持 1 轮派生的最小实现（不引 golang.org/x/crypto）。
// 1 轮时 PBKDF2 就是 HMAC-SHA1(password, salt||0x00000001) 取前 keyLen 字节。
func pbkdf2SHA1Key(password, salt []byte, rounds, keyLen int) []byte {
	mac := hmac.New(sha1.New, password)
	mac.Write(salt)
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], 1)
	mac.Write(idx[:])
	u := mac.Sum(nil)

	out := make([]byte, 0, keyLen)
	out = append(out, u...)
	prev := u
	for r := 2; r <= rounds; r++ {
		m := hmac.New(sha1.New, password)
		m.Write(prev)
		prev = m.Sum(nil)
		for i := 0; i < len(out); i++ {
			out[i] ^= prev[i]
		}
	}
	if len(out) > keyLen {
		out = out[:keyLen]
	}
	return out
}

// teleagentDecryptToken 解 token.json 里的 encryptedToken（纯函数，便于单测）。
func teleagentDecryptToken(encBase64, password string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encBase64))
	if err != nil {
		return "", fmt.Errorf("encryptedToken 不是合法 base64：%v", err)
	}
	// Chromium OSCrypt：3 字节版本前缀（v10/v11）+ 密文
	if len(raw) <= 3 {
		return "", errors.New("encryptedToken 太短")
	}
	prefix := string(raw[:3])
	if prefix != "v10" && prefix != "v11" {
		return "", fmt.Errorf("不认识的加密前缀 %q（只处理 Chromium v10/v11）", prefix)
	}
	key := pbkdf2SHA1Key([]byte(password), []byte(teleagentSalt), teleagentKDFRounds, teleagentKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	ct := raw[3:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", errors.New("密文长度不是 AES 块大小的整数倍")
	}
	iv := bytes.Repeat([]byte(" "), aes.BlockSize) // Chromium：IV = 16 个空格
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	// 去 PKCS#7 填充
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(plain) {
		return "", errors.New("解密后 padding 非法（大概率口令不对）")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return "", errors.New("解密后 padding 非法（大概率口令不对）")
		}
	}
	plain = plain[:len(plain)-pad]

	// 明文形态实测是 "<标识>:<JWT>"；没有冒号就当整段是 token。
	text := string(plain)
	if i := strings.Index(text, ":"); i > 0 && i < 64 {
		if rest := text[i+1:]; strings.HasPrefix(rest, "eyJ") || len(rest) > 100 {
			return rest, nil
		}
	}
	return text, nil
}

// TeleagentToken 取 token（带 30 分钟内存缓存）。
func teleagentToken() (string, error) {
	teleagentTokenState.Lock()
	defer teleagentTokenState.Unlock()
	if teleagentTokenState.token != "" && time.Since(teleagentTokenState.fetched) < 30*time.Minute {
		return teleagentTokenState.token, nil
	}
	path, err := teleagentTokenPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读 token.json 失败：%v", err)
	}
	var f struct {
		Scheme         string `json:"scheme"`
		EncryptedToken string `json:"encryptedToken"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return "", fmt.Errorf("token.json 解析失败：%v", err)
	}
	if f.EncryptedToken == "" {
		return "", errors.New("token.json 里没有 encryptedToken")
	}
	pw, err := teleagentSafeStoragePassword()
	if err != nil {
		return "", err
	}
	token, err := teleagentDecryptToken(f.EncryptedToken, pw)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("解出来的 token 为空")
	}
	teleagentTokenState.token = token
	teleagentTokenState.fetched = time.Now()
	return token, nil
}

// teleagentForgetToken 清缓存（401 换 token 时用）。
func teleagentForgetToken() {
	teleagentTokenState.Lock()
	teleagentTokenState.token = ""
	teleagentTokenState.fetched = time.Time{}
	teleagentTokenState.Unlock()
}
