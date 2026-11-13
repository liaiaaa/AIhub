package main

// 华为云果办（OfficeAce）签名层：两套华为网关签名。
//
// 实测来源（2026-11-12/13，vault 真凭据打真端点）：
//   - SDK-HMAC-SHA256：agentarts.cn-southwest-2.myhuaweicloud.com（SKU/下单/
//     client-subscription/permission-validate/iam）与 iam.myhuaweicloud.com。
//     密钥 = 裸 SK 做 HMAC；canonicalUri 末尾补 /；带 X-Security-Token。
//   - V11-HMAC-SHA256：officeace.cn-southwest-2.huaweicloud-agentarts.com
//     （订阅面：额度/倍率/签到）。密钥派生两跳：
//     u1 = HMAC-SHA256(AK, SK)
//     dk = HMAC-SHA256(u1, scope + 0x01)[:32] 的 hex 字符串（再按 utf8 当密钥）
//     scope = yyyymmdd/cn-southwest-2/apic；算法名 "V11-HMAC-SHA256"。
//     canonicalHeaders 每行 "name:value\n"；canonicalUri 也补斜杠。
//
// 注意两处容易写错的细节（Node 与 Go 对照实测）：
//   1. canonicalUri 的每个路径段先 decode 再 encode（华为的 encode 保留 -_.~，
//      对 !'()* 做 %XX 大写转义——与 RFC3986 的 unreserved 集合不同）。
//   2. V11 的派生密钥 dk 是 hex 字符串本身当 UTF-8 字节用（不是二进制再取 32），
//      signV11 的 HMAC key = []byte(hexString)。SDK-HMAC 的 key = SK 原文。
//
// 这两个函数只做签名，不发请求；请求由调用方发（签到/额度/网关各自带超时与降级）。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	hwRegion  = "cn-southwest-2" // 果办所有已验证端点都在这个 region
	hwService = "apic"
)

// hwSdkDate 华为 X-Sdk-Date 格式：yyyymmddTHHMMSSZ（UTC）。
func hwSdkDate(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d%02d%02dT%02d%02d%02dZ",
		t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
}

// hwURLEncode 华为签名的路径段编码：与 RFC3986 的差异是 !'()* 也要转义。
func hwURLEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// hwCanonicalURI 路径规范：逐段 decode→encode，末尾补 /。
func hwCanonicalURI(path string) string {
	segs := strings.Split(path, "/")
	out := make([]string, len(segs))
	for i, seg := range segs {
		if dec, err := url.PathUnescape(seg); err == nil {
			out[i] = hwURLEncode(dec)
		} else {
			out[i] = hwURLEncode(seg)
		}
	}
	uri := strings.Join(out, "/")
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	return uri
}

// hwCanonicalQuery 排序后的规范化查询串。
func hwCanonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := append([]string{}, q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, hwURLEncode(k)+"="+hwURLEncode(v))
		}
	}
	return strings.Join(pairs, "&")
}

// hwSTS 果办会话凭据（来自 vault：~/.dsh/.huaweicloud-connect/officeace-vault/）。
type hwSTS struct {
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	// ExpiresAt 凭据到期时刻。续期后必须带着新值一起落盘，否则下次读 vault
	// 仍看到旧的过期时间 → 反复续期 → refresh_token 一次性 → 第二次必失败。
	ExpiresAt time.Time
}

// signHuaweiSDK SDK-HMAC-SHA256（agentarts/iam 域）。
//
// headers：业务头（content-type / x-subscription-type / x-project-id 等），
// 函数内补 X-Sdk-Date / X-Sdk-Content-Sha256 / X-Security-Token / host。
// 返回可直接用于请求的完整头表（含 Authorization）。
func signHuaweiSDK(cred hwSTS, method, rawURL string, body []byte, headers map[string]string) map[string]string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	xd := hwSdkDate(time.Now())
	payloadHash := hex.EncodeToString(sha256sum(body))

	h := map[string]string{}
	for k, v := range headers {
		h[k] = v
	}
	h["X-Sdk-Date"] = xd
	h["X-Sdk-Content-Sha256"] = payloadHash
	if cred.SecurityToken != "" {
		h["X-Security-Token"] = cred.SecurityToken
	}

	names := make([]string, 0, len(h)+1)
	lower := map[string]string{}
	for k, v := range h {
		k = strings.ToLower(k)
		v = strings.TrimSpace(v)
		lower[k] = v
		names = append(names, k)
	}
	lower["host"] = u.Host
	names = append(names, "host")
	sort.Strings(names)

	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + lower[n] + "\n")
	}
	cr := strings.Join([]string{
		strings.ToUpper(method),
		hwCanonicalURI(u.Path),
		hwCanonicalQuery(u.Query()),
		ch.String(),
		strings.Join(names, ";"),
		payloadHash,
	}, "\n")
	sts := strings.Join([]string{"SDK-HMAC-SHA256", xd, hex.EncodeToString(sha256sum([]byte(cr)))}, "\n")
	sig := hex.EncodeToString(hmacSHA256([]byte(cred.SecretAccessKey), []byte(sts)))

	h["Authorization"] = fmt.Sprintf("SDK-HMAC-SHA256 Access=%s, SignedHeaders=%s, Signature=%s",
		cred.AccessKeyID, strings.Join(names, ";"), sig)
	return h
}

// signHuaweiV11 V11-HMAC-SHA256（officeace 订阅面：额度/倍率/签到）。
//
// 订阅面固定头：x-subscription-type: v2、X-Sdk-Date、X-Security-Token、Host
// （POST 再加 Content-Type: application/json;charset=utf8）。业务头通过
// extra 传入并一并参与签名。
func signHuaweiV11(cred hwSTS, method, rawURL string, body []byte, extra map[string]string) map[string]string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	xd := hwSdkDate(time.Now())
	scope := fmt.Sprintf("%s/%s/%s", xd[:8], hwRegion, hwService)

	original := map[string]string{}
	for k, v := range extra {
		original[k] = v
	}
	original["x-subscription-type"] = "v2"
	original["X-Sdk-Date"] = xd
	original["X-Security-Token"] = strings.TrimSpace(cred.SecurityToken)
	original["Host"] = u.Host
	if strings.ToUpper(method) == "POST" {
		original["Content-Type"] = "application/json;charset=utf8"
	}

	lower := map[string]string{}
	for k, v := range original {
		lower[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	names := make([]string, 0, len(lower))
	for k := range lower {
		names = append(names, k)
	}
	sort.Strings(names)

	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + lower[n] + "\n")
	}
	cr := strings.Join([]string{
		strings.ToUpper(method),
		hwCanonicalURI(u.Path),
		hwCanonicalQuery(u.Query()),
		ch.String(),
		strings.Join(names, ";"),
		hex.EncodeToString(sha256sum(body)),
	}, "\n")
	stringToSign := strings.Join([]string{
		"V11-HMAC-SHA256", xd, scope,
		hex.EncodeToString(sha256sum([]byte(cr))),
	}, "\n")

	u1 := hmacSHA256([]byte(cred.AccessKeyID), []byte(cred.SecretAccessKey))
	// 密钥派生（Node 对照实测）：dkBuf = HMAC(u1, scope+0x01).digest() 取前 32 字节，
	// 再 .toString('hex') 得 64 字符 hex；HMAC key = 该 hex 字符串的 utf8 字节。
	dkFull := hmacSHA256(u1, append([]byte(scope), 0x01))
	hexKey := hex.EncodeToString(dkFull[:32])
	sig := hex.EncodeToString(hmacSHA256([]byte(hexKey), []byte(stringToSign)))

	out := map[string]string{}
	for k, v := range original {
		out[k] = v
	}
	out["Authorization"] = fmt.Sprintf("V11-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cred.AccessKeyID, scope, strings.Join(names, ";"), sig)
	return out
}

func sha256sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
