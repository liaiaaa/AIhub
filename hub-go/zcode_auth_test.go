package main

// Zcode 桌面端凭据解密测试。
//
// 真实凭据文件不宜进测试；这里用 ZCODE_CREDENTIAL_SECRET 走自定义密钥路径，
// 在临时目录造一份 credentials.json，锁定 enc:v1 信封的加解密往返与条目选取。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// zcodeTestEncrypt 用指定密钥造一个 enc:v1 信封（与桌面端 encrypt 逻辑一致）。
func zcodeTestEncrypt(t *testing.T, key []byte, plaintext string) string {
	t.Helper()
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("rand iv: %v", err)
	}
	cb, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(cb)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	ct := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tag := ct[len(ct)-16:]
	body := ct[:len(ct)-16]
	return "enc:v1:" + base64.RawURLEncoding.EncodeToString(iv) +
		"." + base64.RawURLEncoding.EncodeToString(tag) +
		"." + base64.RawURLEncoding.EncodeToString(body)
}

func TestZcodeCredentialDecryptAndPick(t *testing.T) {
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "unit-test-secret")
	key := zcodeCipherKey()

	creds := map[string]string{
		"oauth:zai:access_token": zcodeTestEncrypt(t, key, "jwt-access-token"),
		"oauth:active_provider":  zcodeTestEncrypt(t, key, "zai"),
		"account-provider:coding-plan:account:zai-individual-coding-plan:account:uuid-1:api-key":
			zcodeTestEncrypt(t, key, "0123abcd.secret1234"),
		"oauth:zai:user_info": zcodeTestEncrypt(t, key,
			`{"user_id":"uuid-1","email":"t@example.com","name":"tester"}`),
	}
	raw, _ := json.Marshal(creds)
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	t.Setenv("ZCODE_CREDENTIALS_PATH", path)

	a, err := NewZcodeAuth()
	if err != nil {
		t.Fatalf("NewZcodeAuth: %v", err)
	}
	if got := a.Key(); got != "0123abcd.secret1234" {
		t.Errorf("Key() = %q, want coding-plan api key", got)
	}
	s := a.Summary()
	if s["user_email"] != "t@example.com" || s["active_provider"] != "zai" {
		t.Errorf("Summary 账号信息不符: %v", s)
	}
	if head, _ := s["key_head"].(string); strings.Contains(head, "secret1234") {
		t.Errorf("key_head 应脱敏, 实际 %q", head)
	}
}

// 密钥不对（比如换了 Windows 用户）时报错清晰，而不是返回半截数据。
func TestZcodeCredentialWrongKeyFails(t *testing.T) {
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "unit-test-secret")
	key := zcodeCipherKey()
	creds := map[string]string{
		"account-provider:coding-plan:account:x:api-key": zcodeTestEncrypt(t, key, "k"),
	}
	raw, _ := json.Marshal(creds)
	path := filepath.Join(t.TempDir(), "credentials.json")
	_ = os.WriteFile(path, raw, 0600)
	t.Setenv("ZCODE_CREDENTIALS_PATH", path)
	t.Setenv("ZCODE_CREDENTIAL_SECRET", "different-secret") // 换密钥

	if _, err := NewZcodeAuth(); err == nil {
		t.Fatal("密钥不匹配应报错")
	}
}

// 回退 secret 的拼串格式锁定（Node os.platform/homedir/username 对齐）。
func TestZcodeFallbackSecretFormat(t *testing.T) {
	// platform 映射对齐 Node os.platform()：windows→"win32"
	if runtime.GOOS == "windows" && zcodeNodePlatform() != "win32" {
		t.Errorf("platform = %q, want win32", zcodeNodePlatform())
	}
	if name := zcodeCurrentUserName(); name == "" || strings.Contains(name, "\\") {
		t.Errorf("username 应剥离域前缀, 实际 %q", name)
	}
	// 回退串格式：zcode-credential-fallback:{platform}:{home}:{user}
	home, _ := os.UserHomeDir()
	key := zcodeCipherKey()
	want := sha256.Sum256([]byte(
		"zcode-credential-fallback:" + zcodeNodePlatform() + ":" + home + ":" + zcodeCurrentUserName()))
	if string(key) != string(want[:]) {
		t.Error("回退密钥派生格式不符")
	}
}
