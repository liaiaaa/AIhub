package main

// WorkBuddy 5.7.6 字段信封的往返测试。
// 第一个用例自给自足（随机密钥 + 临时数据根），第二个用例只在机器上真的有
// 桌面端凭据和 wbatrest.key 时才跑，否则跳过——CI 上不会失败。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempKey 把数据根指向临时目录并写入一把随机密钥，返回清理函数。
func withTempKey(t *testing.T) ([]byte, func()) {
	t.Helper()
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wbKeyFileName), []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	*wbCodecInstance = wbCodec{}
	return key, func() {
		hubRoot = old
		*wbCodecInstance = wbCodec{}
	}
}

func TestWBAtRestRoundTrip(t *testing.T) {
	key, cleanup := withTempKey(t)
	defer cleanup()

	meta := wbFieldMeta{KeyID: wbKeyIDOf(key), Suite: 1, Framing: "field"}
	doc := map[string]any{
		"account": map[string]any{"uid": "u-1", "nickname": "plain-stays"},
		"auth":    map[string]any{"domain": "www.codebuddy.cn"},
	}
	sealed, err := wbCodecInstance.seal(meta, "Gin")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	doc["account"].(map[string]any)["nickname"] = sealed
	// 桌面端的写法：信封是 {"$wbEncrypted":1,"envelope":"<b64>"}
	if _, ok := asEnvelope(sealed); !ok {
		t.Fatalf("seal 出来的不是信封形状：%v", sealed)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	plain, metas, err := wbUnsealBytes(b)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("应记录 1 处信封，实际 %d：%v", len(metas), metas)
	}
	if _, ok := metas[".account.nickname"]; !ok {
		t.Fatalf("信封位置记错：%v", metas)
	}
	var s SessionFile
	if err := json.Unmarshal(plain, &s); err != nil {
		t.Fatalf("解封后仍无法按结构体解析：%v\n%s", err, plain)
	}
	if s.Account == nil || s.Account.Nickname != "Gin" {
		t.Fatalf("nickname 解错：%+v", s.Account)
	}
	if s.Account.UID != "u-1" {
		t.Fatalf("未加密字段被改动：%+v", s.Account)
	}

	// 回封：明文字段重新变回信封，且再解一次仍是原值
	again, err := json.MarshalIndent(map[string]any{
		"account": map[string]any{"uid": "u-1", "nickname": "Gin"},
		"auth":    map[string]any{"domain": "www.codebuddy.cn"},
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	re, err := wbResealBytes(again, metas)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if !strings.Contains(string(re), wbEncMarker) {
		t.Fatalf("回封后没有信封：%s", re)
	}
	var back map[string]any
	if err := json.Unmarshal(re, &back); err != nil {
		t.Fatal(err)
	}
	env := back["account"].(map[string]any)["nickname"].(map[string]any)
	pt, err := wbCodecInstance.open("field", env["envelope"].(string))
	if err != nil {
		t.Fatalf("回封的信封解不开：%v", err)
	}
	if pt != "Gin" {
		t.Fatalf("往返结果不等价：%q", pt)
	}
}

func TestWBAtRestPlainFileUntouched(t *testing.T) {
	b := []byte(`{"account":{"nickname":"hli"},"auth":{"accessToken":"eyJx"}}`)
	out, meta, err := wbUnsealBytes(b)
	if err != nil || meta != nil || string(out) != string(b) {
		t.Fatalf("明文文件应原样通过：meta=%v err=%v out=%s", meta, err, out)
	}
}

// TestWBAtRestRealCredential 校验真实桌面端凭据能解开。没有材料就跳过。
func TestWBAtRestRealCredential(t *testing.T) {
	authPath := NewDomProxy().AuthFilePath()
	b, err := os.ReadFile(authPath)
	if err != nil {
		t.Skipf("没有国内凭据文件：%v", err)
	}
	if !strings.Contains(string(b), wbEncMarker) {
		t.Skipf("凭据文件是明文，无需解密：%s", authPath)
	}
	if _, err := os.Stat(wbKeyPath()); err != nil {
		t.Skipf("缺少解密密钥 %s", wbKeyPath())
	}
	plain, meta, err := wbUnsealBytes(b)
	if err != nil {
		t.Fatalf("解封失败：%v", err)
	}
	if len(meta) < 4 {
		t.Errorf("预期至少 4 处信封（nickname/phoneNumber/accessToken/refreshToken），实际 %d", len(meta))
	}
	var s SessionFile
	if err := json.Unmarshal(plain, &s); err != nil {
		t.Fatalf("结构体解析失败：%v", err)
	}
	if s.Account == nil || s.Account.Nickname == "" {
		t.Error("nickname 解出来是空的")
	}
	if s.Auth == nil || !strings.HasPrefix(s.Auth.AccessToken, "eyJ") {
		t.Error("accessToken 不像 JWT")
	}
	if s.Auth == nil || s.Auth.RefreshToken == "" {
		t.Error("refreshToken 解不出来")
	}
	// 不把明文写进测试日志，只报长度。
	if s.Auth != nil {
		t.Logf("uid=%s nickname_len=%d access_len=%d refresh_len=%d 信封处数=%d",
			s.Account.UID, len(s.Account.Nickname), len(s.Auth.AccessToken), len(s.Auth.RefreshToken), len(meta))
	}
}

// 密钥环：国内凭据与移植版国际凭据用的是**两把不同的密钥**
// （移植 shim 找不到 .at-rest-key 会 randomBytes 现生成），所以 loadKeys
// 必须同时收多把，换一把不能弄坏另一条线（2026-10-16）。
func TestWBAtRestKeyringBothKeysWork(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	*wbCodecInstance = wbCodec{}
	t.Cleanup(func() { hubRoot = old; *wbCodecInstance = wbCodec{} })

	// 主密钥（国内那把）
	keyA := make([]byte, 32)
	if _, err := rand.Read(keyA); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wbKeyFileName),
		[]byte(base64.StdEncoding.EncodeToString(keyA)), 0o600); err != nil {
		t.Fatal(err)
	}
	// 备用密钥（移植版国际那把）：按桌面端规则写成 44 字符原文，
	// 真正的 AES key 是 sha256(字符串)，正好覆盖 loadKeys 的第二种候选。
	rawB := make([]byte, 32)
	if _, err := rand.Read(rawB); err != nil {
		t.Fatal(err)
	}
	b64B := base64.StdEncoding.EncodeToString(rawB)
	derivedB := sha256.Sum256([]byte(b64B))
	if err := os.WriteFile(filepath.Join(dir, "wbatrest-workbuddy-ai.key"),
		[]byte(b64B), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		meta wbFieldMeta
		want string
	}{
		{"主密钥", wbFieldMeta{KeyID: wbKeyIDOf(keyA), Suite: 1, Framing: "field"}, "国内昵称"},
		{"备用密钥(sha256派生)", wbFieldMeta{KeyID: wbKeyIDOf(derivedB[:]), Suite: 1, Framing: "field"}, "国际昵称"},
	} {
		env, err := wbCodecInstance.seal(tc.meta, tc.want)
		if err != nil {
			t.Fatalf("%s seal: %v", tc.name, err)
		}
		e, ok := asEnvelope(env)
		if !ok {
			t.Fatalf("%s 出来的不是信封", tc.name)
		}
		got, err := wbCodecInstance.open(tc.meta.Framing, e)
		if err != nil {
			t.Fatalf("%s open: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s 解出来 %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

// 主文件缺失但有备用密钥时，不该直接判定「没有密钥」。
func TestWBAtRestWorksWhenPrimaryKeyMissing(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	*wbCodecInstance = wbCodec{}
	t.Cleanup(func() { hubRoot = old; *wbCodecInstance = wbCodec{} })

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wbatrest-port.key"),
		[]byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := wbCodecInstance.seal(wbFieldMeta{KeyID: wbKeyIDOf(key), Suite: 1, Framing: "field"}, "只有备用密钥")
	if err != nil {
		t.Fatalf("wbatrest.key 不存在时也应能用备用密钥： %v", err)
	}
	e, _ := asEnvelope(env)
	if got, err := wbCodecInstance.open("field", e); err != nil || got != "只有备用密钥" {
		t.Fatalf("解回失败：%v %q", err, got)
	}
}

// 一把都没有时才报错，且提示要提到两种文件名。
func TestWBAtRestNoKeysErrors(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	*wbCodecInstance = wbCodec{}
	t.Cleanup(func() { hubRoot = old; *wbCodecInstance = wbCodec{} })

	if _, err := wbCodecInstance.seal(wbFieldMeta{KeyID: "x", Suite: 1, Framing: "field"}, "v"); err == nil {
		t.Fatal("没有任何密钥文件时必须报错")
	}
}
