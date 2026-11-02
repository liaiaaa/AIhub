package main

// 2026-11-02：TeleAgent safeStorage 解密链路的回归锁定。
//
// 这条链路是纯本地算法，可以完全离线测试：
//   PBKDF2-HMAC-SHA1(password,"saltysalt",1 轮,16B) → AES-128-CBC(IV=16 空格)

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestPBKDF2SHA1KnownVector RFC 6070 第 2 组：P="password" S="salt" c=1 dkLen=20。
func TestPBKDF2SHA1KnownVector(t *testing.T) {
	got := hex.EncodeToString(pbkdf2SHA1Key([]byte("password"), []byte("salt"), 1, 20))
	want := "0c60c80f961f0e71f3a9b524af6012062fe037a6"
	if got != want {
		t.Fatalf("PBKDF2 结果不对：\n got=%s\nwant=%s", got, want)
	}
	// 取前 16 字节（Electron 用的长度）必须是同一前缀
	got16 := hex.EncodeToString(pbkdf2SHA1Key([]byte("password"), []byte("salt"), 1, 16))
	if !strings.HasPrefix(want, got16) {
		t.Fatalf("16 字节派生不是 20 字节结果的前缀：%s", got16)
	}
}

// encryptLikeChromium 按 Chromium OSCrypt 的形态造一个 v11 密文（测试用）。
func encryptLikeChromium(t *testing.T, plain, password string) string {
	t.Helper()
	key := pbkdf2SHA1Key([]byte(password), []byte(teleagentSalt), teleagentKDFRounds, teleagentKeyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append([]byte(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte(" "), aes.BlockSize)).CryptBlocks(out, buf)
	return base64.StdEncoding.EncodeToString(append([]byte("v11"), out...))
}

func TestTeleagentDecryptTokenRoundTrip(t *testing.T) {
	const pw = "kBCAqbZxNAdJX6XhG25NSg==" // 形态与实际 keyring secret 一致，值仅测试用
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ0ZXN0In0.c2ln"
	enc := encryptLikeChromium(t, "Em9xR4:"+jwt, pw)

	got, err := teleagentDecryptToken(enc, pw)
	if err != nil {
		t.Fatalf("解密失败：%v", err)
	}
	if got != jwt {
		t.Fatalf("应剥掉「标识:」前缀只留 JWT，实际 %q", got)
	}

	// 口令不对必须报错，不能返回垃圾
	if _, err := teleagentDecryptToken(enc, "wrong-password"); err == nil {
		t.Error("错误口令应当解密失败")
	}
	// 不认识的版本前缀要明确报错
	bad := base64.StdEncoding.EncodeToString(append([]byte("v99"), make([]byte, 32)...))
	if _, err := teleagentDecryptToken(bad, pw); err == nil {
		t.Error("v99 前缀应当被拒绝")
	}
}

func TestTeleagentDecryptWithoutPrefix(t *testing.T) {
	// 没有冒号时整段就是 token（防上游换格式后直接崩）
	enc := encryptLikeChromium(t, "plain-token-value", "pw")
	got, err := teleagentDecryptToken(enc, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain-token-value" {
		t.Fatalf("实际 %q", got)
	}
}
