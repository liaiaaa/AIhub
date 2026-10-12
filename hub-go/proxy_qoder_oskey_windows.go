//go:build windows

package main

// Qoder 桌面端凭据解密（Windows 路线）：
// Local State 里的 os_crypt.encrypted_key → DPAPI 解出 32 字节主密钥
// → auth.v1.dat（v10 + 12B nonce + AES-256-GCM 密文+tag）。

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

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

// qoderOpenCreds 把 auth.v1.dat 解成凭据 JSON 明文，第二个返回值是生效的解密方式。
func qoderOpenCreds(base string, dat []byte) ([]byte, string, error) {
	lsBytes, err := os.ReadFile(filepath.Join(base, "Local State"))
	if err != nil {
		return nil, "", fmt.Errorf("读取 Local State 失败: %w", err)
	}
	var ls struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(lsBytes, &ls); err != nil {
		return nil, "", fmt.Errorf("解析 Local State 失败: %w", err)
	}
	encKey, err := base64.StdEncoding.DecodeString(ls.OsCrypt.EncryptedKey)
	if err != nil || len(encKey) < 5 {
		return nil, "", fmt.Errorf("无效的 encrypted_key")
	}
	aesKey, err := qCryptUnprotect(encKey[5:]) // 去掉 5 字节 "DPAPI" 前缀
	if err != nil {
		return nil, "", fmt.Errorf("DPAPI 解密主密钥失败: %w", err)
	}

	if len(dat) < 3+12+16 || string(dat[:3]) != "v10" {
		return nil, "", fmt.Errorf("auth.v1.dat 不是有效的 safeStorage 格式")
	}
	cb, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, "", fmt.Errorf("创建 AES Cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(cb)
	if err != nil {
		return nil, "", fmt.Errorf("创建 GCM 失败: %w", err)
	}
	plain, err := gcm.Open(nil, dat[3:15], dat[15:], nil)
	if err != nil {
		return nil, "", fmt.Errorf("AES-GCM 解密 auth.v1.dat 失败: %w", err)
	}
	return plain, "dpapi+aes256gcm", nil
}
