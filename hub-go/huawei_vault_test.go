package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHwWriteBackUpdatesExpiry(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "acc_test.json")
	orig := map[string]any{
		"schemaVersion": 1,
		"account": map[string]any{
			"id": "acc_test", "label": "dom123",
			"sts": map[string]any{
				"accessKeyId": "OLD_AK", "secretAccessKey": "OLD_SK",
				"securityToken": "OLD_TOK", "refreshToken": "OLD_RT",
				"expiresAt":      "2026-01-01T00:00:00Z",
				"dpopPrivateJwk": map[string]any{"kty": "EC", "crv": "P-256"},
			},
		},
	}
	b, _ := json.Marshal(orig)
	os.WriteFile(f, b, 0o600)

	newExp := time.Now().Add(90 * time.Minute).UTC()
	cred := hwSTS{AccessKeyID: "NEW_AK", SecretAccessKey: "NEW_SK", SecurityToken: "NEW_TOK", ExpiresAt: newExp}
	if err := hwWriteBackRefresh(f, cred, "NEW_RT"); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	raw, _ := os.ReadFile(f)
	var got map[string]any
	json.Unmarshal(raw, &got)
	sts := got["account"].(map[string]any)["sts"].(map[string]any)

	if sts["accessKeyId"] != "NEW_AK" {
		t.Errorf("accessKeyId 未更新: %v", sts["accessKeyId"])
	}
	if sts["refreshToken"] != "NEW_RT" {
		t.Errorf("refreshToken 未更新: %v", sts["refreshToken"])
	}
	expStr, _ := sts["expiresAt"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, expStr)
	if err != nil {
		t.Fatalf("expiresAt 格式坏了: %q", expStr)
	}
	if !parsed.After(time.Now().Add(80 * time.Minute)) {
		t.Errorf("expiresAt 没更新到未来: %s（这会导致反复续期把账号锁死）", expStr)
	}
	// dpop 私钥必须原样保留（与 refresh_token 绑定）
	if got["account"].(map[string]any)["sts"].(map[string]any)["dpopPrivateJwk"] == nil {
		t.Error("dpopPrivateJwk 被丢了")
	}
	t.Logf("✓ 写回正确：expiresAt=%s，dpop 私钥保留", expStr)
}
