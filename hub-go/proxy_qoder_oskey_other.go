//go:build !windows

package main

// Qoder 桌面端凭据解密（非 Windows 路线）。
//
// Linux 上没有 DPAPI：Electron/Chromium 的 safeStorage 主口令来自系统密钥环
// （GNOME Keyring / libsecret，经 Secret Service DBus），密钥环不可用时退化成
// 硬编码口令 "peanuts"（相当于 --password-store=basic）。
//
// 本机（Qoder CN / Ubuntu）2026-10-09 实测走通的是：
//   密钥环条目 application="Qoder CN App"（xdg:schema=chrome_libsecret_os_crypt_password_v2）
//   → PBKDF2-HMAC-SHA1(口令, salt="saltysalt", iter=1, 16 字节)
//   → 信封头 **v11** + AES-128-CBC（固定 IV=16 个空格）+ PKCS7
// 与 Windows 的 v10 + AES-256-GCM 是两套形态。版本头、密钥长度、分组/GCM 形态
// 都保持多候选轮试，桌面端升级换形态时不必改代码。
//
// 三条免重编的兜底通道：
//   QODER_SAFE_STORAGE_PASSWORD  口令原文（优先于密钥环）
//   QODER_SAFE_STORAGE_KEY       已派生密钥的 base64/hex（16/24/32 字节）
//   QODER_SAFE_STORAGE_APP       密钥环条目 application 属性的取值

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// qoderSafePbkdf2Salt/Iterations 对齐 Chromium Linux 的 KDF 参数。
const (
	qoderSafeSalt       = "saltysalt"
	qoderSafeIterations = 1
)

// qoderOpenCreds 把 auth.v1.dat 解成凭据 JSON 明文，第二个返回值是生效的解密方式。
//
// 版本头按 vNN 通用识别：本机实测是 **v11**（不是 Windows 的 v10），而载荷 400 字节
// 既能当 16 的整数倍（CBC）看、也能当 12B nonce + 密文+tag（GCM）看，
// 所以两种形态 × 全部候选密钥依次试，用「明文是否合法 JSON / GCM 标签是否通过」判定命中。
//
// 注意：vNN 分支绝不裁剪字节——CBC 密文合法地可以以 0x0A 结尾，裁剪会把最后一个
// 分组截掉，表现为「密钥明明对却解不开」。裁剪只用于明文分支。
func qoderOpenCreds(base string, dat []byte) ([]byte, string, error) {
	if !qoderIsVersionedBlob(dat) {
		if plain := bytes.TrimSpace(dat); json.Valid(plain) {
			return plain, "plaintext", nil
		}
		return nil, "", fmt.Errorf("auth.v1.dat 既不是 vNN 信封，也不是明文 JSON（前 3 字节 %q）", string(dat[:min(3, len(dat))]))
	}
	payload := dat[3:]

	keys, sources := qoderSafeStorageKeys()
	for i, k := range keys {
		if plain, err := qoderOpenCBC(k, payload); err == nil {
			return plain, sources[i] + "+aes-cbc", nil
		}
		if plain, err := qoderOpenGCM(k, payload); err == nil {
			return plain, sources[i] + "+aes-gcm", nil
		}
	}
	return nil, "", fmt.Errorf("无法解密 auth.v1.dat，已试过的密钥来源：%s。"+
		"排查顺序：① 装 secret-tool（sudo apt install libsecret-tools）后重试；"+
		"② 用「密码和密钥」(seahorse) 找 %s 之类的条目，把口令填进 QODER_SAFE_STORAGE_PASSWORD，"+
		"或把 16/32 字节密钥的 base64 填进 QODER_SAFE_STORAGE_KEY；"+
		"③ 条目名不同时可设 QODER_SAFE_STORAGE_APP 指定",
		strings.Join(sources, "、"), qoderKeyringAppNames()[0])
}

// qoderIsVersionedBlob 判断是否 Chromium/Electron 的「v + 两位数字」信封头。
func qoderIsVersionedBlob(b []byte) bool {
	return len(b) > 3 && b[0] == 'v' && b[1] >= '0' && b[1] <= '9' && b[2] >= '0' && b[2] <= '9'
}

// qoderSafeStorageKeys 候选主密钥，按可信度排序（显式覆盖 > 密钥环 > peanuts）。
// 每个口令来源展开成三种形态：口令原文即密钥（长度正好 16/24/32 时）、
// PBKDF2 派生 16 字节（v1/CBC 形态）、派生 32 字节（v2 schema 的 GCM 形态）。
func qoderSafeStorageKeys() ([][]byte, []string) {
	var keys [][]byte
	var sources []string
	addKey := func(k []byte, why string) {
		switch len(k) {
		case 16, 24, 32:
			keys = append(keys, k)
			sources = append(sources, why)
		}
	}
	addPassword := func(pw, why string) {
		if pw == "" {
			return
		}
		raw := []byte(pw)
		addKey(raw, why+"(raw)")
		addKey(qoderSafePbkdf2(raw, 16), why+"+pbkdf2-16")
		addKey(qoderSafePbkdf2(raw, 32), why+"+pbkdf2-32")
	}

	if v := strings.TrimSpace(os.Getenv("QODER_SAFE_STORAGE_KEY")); v != "" {
		if raw, err := qoderDecodeKeyMaterial(v); err == nil {
			addKey(raw, "env:QODER_SAFE_STORAGE_KEY")
		}
	}
	if pw := os.Getenv("QODER_SAFE_STORAGE_PASSWORD"); pw != "" {
		addPassword(pw, "env:QODER_SAFE_STORAGE_PASSWORD")
	}
	for _, app := range qoderKeyringAppNames() {
		if pw, ok := qoderKeyringSecret(app); ok {
			addPassword(pw, "keyring:"+app)
		}
	}
	addPassword("peanuts", "peanuts")
	return keys, sources
}

// qoderKeyringAppNames 密钥环里可能的 application 属性取值。
// 第一个是实测值（本机的 login 密钥环条目：Label「Chromium Safe Storage」，
// attributes application="Qoder CN App", xdg:schema=chrome_libsecret_os_crypt_password_v2）。
func qoderKeyringAppNames() []string {
	if v := strings.TrimSpace(os.Getenv("QODER_SAFE_STORAGE_APP")); v != "" {
		return []string{v, v + " Safe Storage"}
	}
	return []string{
		"Qoder CN App",
		"Qoder App",
		"com.qodercn.app.stable Safe Storage",
		"Qoder Safe Storage",
		"Qoder CN Safe Storage",
		"Electron Safe Storage",
	}
}

// qoderKeyringSecret 用 secret-tool 从密钥环取口令；命令缺失或查不到返回 ok=false。
// 先按 application 单属性查（libsecret 走子集匹配），查不到再带上 xdg:schema 精确查。
func qoderKeyringSecret(app string) (string, bool) {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return "", false
	}
	tries := [][]string{{"application", app}}
	for _, schema := range []string{
		"chrome_libsecret_os_crypt_password_v2",
		"chrome_libsecret_os_crypt_password_v1",
	} {
		tries = append(tries, []string{"application", app, "xdg:schema", schema})
	}
	for _, attrs := range tries {
		args := append([]string{"lookup"}, attrs...)
		out, err := execCmd("secret-tool", args...).Output()
		if err != nil {
			continue
		}
		if s := strings.TrimRight(string(out), "\r\n"); s != "" {
			return s, true
		}
	}
	return "", false
}

// qoderDecodeKeyMaterial 接受 base64（标准/URL）或 hex 编码的原始密钥。
func qoderDecodeKeyMaterial(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return hex.DecodeString(s)
}

// qoderSafePbkdf2 PBKDF2-HMAC-SHA1，盐与迭代次数固定为 Chromium Linux 的取值。
func qoderSafePbkdf2(password []byte, keyLen int) []byte {
	salt := []byte(qoderSafeSalt)
	out := make([]byte, 0, keyLen+sha1.Size)
	for block := 1; len(out) < keyLen; block++ {
		be := []byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)}
		mac := hmac.New(sha1.New, password)
		mac.Write(salt)
		mac.Write(be)
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < qoderSafeIterations; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// qoderOpenCBC v10 载荷 = AES-CBC(IV=16 个空格) + PKCS7；无认证标签，
// 用「明文是否为合法 JSON」判断密钥对不对。
func qoderOpenCBC(key, payload []byte) ([]byte, error) {
	if len(key) == 0 || len(payload) == 0 || len(payload)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("长度不符")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	plain := make([]byte, len(payload))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(plain, payload)
	plain = traeUnpadPKCS7(plain)
	if !json.Valid(plain) {
		return nil, fmt.Errorf("解密结果不是合法 JSON")
	}
	return plain, nil
}

// qoderOpenGCM v10 载荷 = 12B nonce + 密文+tag（Windows/新版形态）。
func qoderOpenGCM(key, payload []byte) ([]byte, error) {
	if len(payload) < 12+16 {
		return nil, fmt.Errorf("载荷过短")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, payload[:12], payload[12:], nil)
}
