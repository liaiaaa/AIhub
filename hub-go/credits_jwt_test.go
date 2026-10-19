package main

// 2026-10-19：Trae 额度查询报「HTTP 401」时，用户不知道是哪里坏了。
// 实测（2026-10-19 15:09）发现是 JWT 登录态过期：
//   iat = 2026-10-05 14:14:58
//   exp = 2026-10-19 14:14:58   ← 已过期 54 分钟
// 上游返回 {"code":1001,"message":"not able to authenticate you"}。
//
// 现在 401 时解出 exp，直接告诉用户「登录态什么时候到期的、该怎么修」。

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// 造一个只有 exp 声明的 JWT（不需要真签名，jwtExpiry 不验签）。
func makeJWT(t *testing.T, exp int64) string {
	t.Helper()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"exp": exp, "iat": exp - 1209600})
	body := base64.RawURLEncoding.EncodeToString(payload)
	return hdr + "." + body + ".fakesignature"
}

func TestJWTExpiryParsesExp(t *testing.T) {
	want := time.Date(2026, 10, 19, 14, 14, 58, 0, time.Local)
	tok := makeJWT(t, want.Unix())
	got, ok := jwtExpiry(tok)
	if !ok {
		t.Fatal("应能解出 exp")
	}
	if !got.Equal(want) {
		t.Errorf("exp = %v，应为 %v", got, want)
	}
}

func TestJWTExpiryRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"not-a-jwt",
		"only.two",
		"a.b.c.d",              // 段数不对
		"aaa.!!!invalid!!!.ccc", // base64 解不开
	}
	for _, c := range cases {
		if _, ok := jwtExpiry(c); ok {
			t.Errorf("垃圾 token %q 不该解出 exp", c)
		}
	}
	// 合法 JWT 但没有 exp
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	pl := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	if _, ok := jwtExpiry(hdr + "." + pl + ".sig"); ok {
		t.Error("没有 exp 声明时不该返回 ok")
	}
}

// 过期时间要能正确判断「已过期」——这是给用户看的核心信息。
func TestJWTExpiryDetectsExpired(t *testing.T) {
	past := time.Now().Add(-54 * time.Minute)
	tok := makeJWT(t, past.Unix())
	exp, ok := jwtExpiry(tok)
	if !ok {
		t.Fatal("应能解出 exp")
	}
	if !exp.Before(time.Now()) {
		t.Error("54 分钟前的 exp 应判为已过期")
	}
}
