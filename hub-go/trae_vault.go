package main

// Trae（字节 AI IDE）本地凭据解密与读取。
//
// Trae 桌面端不用 DPAPI/safeStorage，而是自研 "tc" 信封（纯算法、可离线解）：
//
//	密文 = base64( [6B 头][32B 随机数][AES-128-CBC 密文] )
//	明文 = [64B SHA-512 校验][JSON]
//	KDF  = n = SHA512(random32 ‖ salt(64))；aesKey = n[0:16]，iv = n[16:32]
//	salt = SALT_A⊕SALT_B（头 74 63 05 10 00 00，普通 AES）
//	       或 SALT_C⊕SALT_D（头 12 39 20 20 02 03，AES_PRIVATE）
//
// 盐表硬编码在 Trae 客户端前端 JS 里，此处转录自开源实现
// laojichao/trae-local-api src/trae-decrypt.js 与
// yembors64632/dsh-connect scripts/trae-vault.mjs（两源一致）。
//
// storage.json（%APPDATA%\Trae CN\User\globalStorage\storage.json）关键键：
//   - iCubeAuthInfo://icube.cloudide —— 登录 JWT（tc 加密，JSON 含
//     token/refreshToken/userId/host）
//   - iCubeAuthInfo://icube-dc:<数字> —— Aha 设备号就在 key 名里（明文）
//   - telemetry.machineId —— 机器标识（作 X-Machine-Id）
//
// 签到/对话接口都以 `Authorization: Cloud-IDE-JWT <jwt>` 认证，且签到必须带
// 真实 x-device-id（编造会稳定返回 9074）。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var traeSaltA = [64]byte{
	82, 9, 106, 213, 48, 54, 165, 56, 191, 64, 163, 158, 129, 243, 215, 251,
	124, 227, 57, 130, 155, 47, 255, 135, 52, 142, 67, 68, 196, 222, 233, 203,
	84, 123, 148, 50, 166, 194, 35, 61, 238, 76, 149, 11, 66, 250, 195, 78,
	8, 46, 161, 102, 40, 217, 36, 178, 118, 91, 162, 73, 109, 139, 209, 37,
}

var traeSaltB = [64]byte{
	31, 221, 168, 51, 136, 7, 199, 49, 177, 18, 16, 89, 39, 128, 236, 95,
	96, 81, 127, 169, 25, 181, 74, 13, 45, 229, 122, 159, 147, 201, 156, 239,
	160, 224, 59, 77, 174, 42, 245, 176, 200, 235, 187, 60, 131, 83, 153, 97,
	23, 43, 4, 126, 186, 119, 214, 38, 225, 105, 20, 99, 85, 33, 12, 125,
}

var traeSaltC = [64]byte{
	191, 192, 216, 250, 122, 246, 220, 97, 31, 254, 98, 27, 8, 72, 71, 176,
	135, 99, 96, 18, 127, 101, 203, 104, 211, 102, 191, 125, 37, 72, 150, 156,
	51, 229, 121, 35, 17, 153, 141, 177, 110, 131, 150, 128, 172, 255, 254, 6,
	18, 140, 55, 62, 236, 249, 135, 64, 135, 12, 117, 4, 89, 149, 168, 209,
}

var traeSaltD = [64]byte{
	246, 204, 26, 232, 232, 70, 129, 109, 223, 146, 169, 242, 23, 241, 105, 145,
	50, 196, 165, 42, 254, 120, 3, 54, 244, 207, 209, 85, 53, 6, 138, 106,
	175, 148, 31, 204, 186, 186, 165, 182, 87, 142, 49, 10, 39, 110, 26, 154,
	86, 56, 173, 125, 18, 64, 198, 225, 99, 99, 83, 82, 191, 134, 76, 170,
}

// traeDecryptValue 解单个 tc 信封值，返回明文 JSON 字节。
func traeDecryptValue(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(raw) < 6+32+64+16 {
		return nil, fmt.Errorf("tc 数据过短（%d 字节）", len(raw))
	}
	var salt [64]byte
	switch {
	case raw[0] == 0x74 && raw[1] == 0x63 && raw[2] == 0x05 && raw[3] == 0x10 && raw[4] == 0x00 && raw[5] == 0x00:
		for i := range salt {
			salt[i] = traeSaltA[i] ^ traeSaltB[i]
		}
	case raw[0] == 18 && raw[1] == 57 && raw[2] == 32 && raw[3] == 32 && raw[4] == 2 && raw[5] == 3:
		for i := range salt {
			salt[i] = traeSaltC[i] ^ traeSaltD[i]
		}
	default:
		return nil, fmt.Errorf("未知的 tc 加密头 % x", raw[:6])
	}
	random := raw[6:38]
	enc := raw[38:]

	h1 := sha512.Sum512(random)
	n := append(h1[:], salt[:]...)
	h2 := sha512.Sum512(n)
	block, err := aes.NewCipher(h2[:16])
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(enc))
	cipher.NewCBCDecrypter(block, h2[16:32]).CryptBlocks(plain, enc)
	plain = traeUnpadPKCS7(plain)
	if len(plain) < 64 {
		return nil, fmt.Errorf("解密结果过短")
	}
	body, sum := plain[64:], plain[:64]
	verify := sha512.Sum512(body)
	if !bytes.Equal(sum, verify[:]) {
		return nil, fmt.Errorf("SHA-512 校验失败（解密不正确）")
	}
	return body, nil
}

func traeUnpadPKCS7(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > 16 || pad > len(b) {
		return b
	}
	for _, c := range b[len(b)-pad:] {
		if int(c) != pad {
			return b
		}
	}
	return b[:len(b)-pad]
}

// traeAuthInfo 解密后的登录态。
type traeAuthInfo struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	UserID       string `json:"userId"`
	Host         string `json:"host"`
	ExpiredAt    string `json:"expiredAt"`
}

// traeUserDataDirs 候选数据目录（CN IDE 优先，SOLO CN 兜底；两者同账号体系、
// 同 chat 端点）。TCR_USER_DATA_DIR 显式指定优先；最后是数据根下的镜像目录，
// 规则与 WorkBuddy 一致：把 Windows 的 %APPDATA%\Trae CN\User 原样拷到
// <数据根>/Trae CN/User 即可（tc 信封是纯算法，跨平台可解，不依赖 DPAPI）。
func traeUserDataDirs() []string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, _ := os.UserHomeDir()
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	dirs := make([]string, 0, 5)
	if v := os.Getenv("TRAE_USER_DATA_DIR"); v != "" {
		dirs = append(dirs, v)
	}
	for _, d := range [][]string{{appData, "Trae CN", "User"}, {appData, "TRAE SOLO CN", "User"},
		{hubRoot, "Trae CN", "User"}, {hubRoot, "TRAE SOLO CN", "User"}} {
		dirs = append(dirs, filepath.Join(d...))
	}
	return dirs
}

// traeDeviceInfo 设备标识（请求头用）：设备号在 key 名里，机器号在 telemetry 里。
type traeDeviceInfo struct {
	DeviceID  string
	MachineID string
}

// traeReadAuth 从桌面端 storage.json 解出登录态与设备号。
// 找不到/未登录返回明确错误。设备号来自 key 名（明文），不依赖解密。
func traeReadAuth() (*traeAuthInfo, traeDeviceInfo, error) {
	// missErr：目录/文件不存在（可以用后面的候选覆盖）；
	// badErr：读到了文件但内容/解密不对（保留第一个——候选目录按可信度排序，
	// 那份才是用户真正放的那份，不能被后面目录的「不存在」盖掉）。
	var missErr, badErr error
	reportBad := func(format string, args ...any) {
		if badErr == nil {
			badErr = fmt.Errorf(format, args...)
		}
	}
	for _, dir := range traeUserDataDirs() {
		path := filepath.Join(dir, "globalStorage", "storage.json")
		b, err := os.ReadFile(path)
		if err != nil {
			missErr = fmt.Errorf("读取 %s 失败: %v", path, err)
			continue
		}
		var storage map[string]json.RawMessage
		if err := json.Unmarshal(b, &storage); err != nil {
			reportBad("%s 解析失败: %v", path, err)
			continue
		}
		enc, ok := storage["iCubeAuthInfo://icube.cloudide"]
		if !ok {
			reportBad("%s 里没有登录凭据（未登录？）", path)
			continue
		}
		var encStr string
		if json.Unmarshal(enc, &encStr) != nil {
			reportBad("%s 里的登录凭据不是字符串", path)
			continue
		}
		var plain []byte
		if strings.HasPrefix(strings.TrimSpace(encStr), "{") {
			plain = []byte(encStr) // SG 版明文存储
		} else if plain, err = traeDecryptValue(encStr); err != nil {
			reportBad("解密 %s 失败: %v", path, err)
			continue
		}
		var info traeAuthInfo
		if err := json.Unmarshal(plain, &info); err != nil {
			reportBad("%s 的凭据 JSON 解析失败: %v", path, err)
			continue
		}
		if info.Token == "" {
			reportBad("%s 解出来的 token 为空（未登录？）", path)
			continue
		}
		dev := traeDeviceInfo{}
		for k := range storage {
			if strings.HasPrefix(k, "iCubeAuthInfo://icube-dc:") {
				dev.DeviceID = strings.TrimPrefix(k, "iCubeAuthInfo://icube-dc:")
			}
			if k == "telemetry.machineId" {
				var m string
				if json.Unmarshal(storage[k], &m) == nil {
					dev.MachineID = m
				}
			}
		}
		return &info, dev, nil
	}
	err := badErr
	if err == nil {
		err = missErr
	}
	if err == nil {
		err = fmt.Errorf("未找到 Trae 桌面端数据目录")
	}
	return nil, traeDeviceInfo{}, fmt.Errorf("%v（桌面端不在默认位置时用 TRAE_USER_DATA_DIR 指定它的 User 目录；"+
		"非 Windows 则把 Trae CN/User 整目录拷到数据根 %s 下，得到 %s/Trae CN/User/globalStorage/storage.json）",
		err, hubRoot, hubRoot)
}
