package main

// tc 解密测试：
//   - 合成用例：按算法正向构造一个信封再解回（不依赖本机登录态）；
//   - 本机用例：真实 storage.json 能解出 token + 设备号（未装 Trae 则跳过）。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	
	"os"
	"path/filepath"
	"testing"
)

func traeSealValue(plaintext []byte, salt [64]byte, header [6]byte) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	h1 := sha512.Sum512(random)
	n := append(h1[:], salt[:]...)
	h2 := sha512.Sum512(n)
	block, err := aes.NewCipher(h2[:16])
	if err != nil {
		return "", err
	}
	payload := make([]byte, 64+len(plaintext))
	copy(payload, func() []byte {
		s := sha512.Sum512(plaintext)
		return s[:]
	}())
	copy(payload[64:], plaintext)
	padLen := 16 - len(payload)%16
	payload = append(payload, bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	enc := make([]byte, len(payload))
	cipher.NewCBCEncrypter(block, h2[16:32]).CryptBlocks(enc, payload)
	out := append([]byte{}, header[:]...)
	out = append(out, random...)
	out = append(out, enc...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func TestTraeVaultRoundTrip(t *testing.T) {
	msg := []byte(`{"token":"abc","userId":"42"}`)
	sealed, err := traeSealValue(msg, traeXorSalts(traeSaltA, traeSaltB), [6]byte{0x74, 0x63, 0x05, 0x10, 0x00, 0x00})
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	got, err := traeDecryptValue(sealed)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("往返不一致: %s", got)
	}

	// AES_PRIVATE 头同样要能解
	sealed2, err := traeSealValue(msg, traeXorSalts(traeSaltC, traeSaltD), [6]byte{18, 57, 32, 32, 2, 3})
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if got2, err := traeDecryptValue(sealed2); err != nil || !bytes.Equal(got2, msg) {
		t.Fatalf("AES_PRIVATE 往返失败: %v", err)
	}

	// 篡改密文必须失败（哈希校验兜底）
	bad, _ := traeSealValue(msg, traeXorSalts(traeSaltA, traeSaltB), [6]byte{0x74, 0x63, 0x05, 0x10, 0x00, 0x00})
	raw, _ := base64.StdEncoding.DecodeString(bad)
	raw[len(raw)-3] ^= 0xff
	if _, err := traeDecryptValue(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatalf("篡改后的密文不应解密成功")
	}
}

func traeXorSalts(a, b [64]byte) [64]byte {
	var out [64]byte
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// TestTraeVaultLocal 本机真实凭据：装了 Trae 并登录过才跑。
func TestTraeVaultLocal(t *testing.T) {
	dir := traeUserDataDirs()[0]
	if _, err := os.Stat(filepath.Join(dir, "globalStorage", "storage.json")); err != nil {
		t.Skipf("本机没有 Trae CN 数据目录，跳过")
	}
	info, dev, err := traeReadAuth()
	if err != nil {
		t.Fatalf("本机凭据解密失败: %v", err)
	}
	if len(info.Token) < 100 {
		t.Fatalf("token 异常短: %d", len(info.Token))
	}
	if dev.DeviceID == "" {
		t.Fatalf("设备号未解析到")
	}
	t.Logf("token len=%d uid=%s device=%s expiredAt=%s", len(info.Token), info.UserID, dev.DeviceID, info.ExpiredAt)
}
