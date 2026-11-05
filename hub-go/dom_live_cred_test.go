package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const liveCredSample = `{"auth":{"accessToken":"tok-new","expiresAt":9999999999999},"account":{"uid":"u1"}}`

func writeFileAt(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDomLiveCredCopiesNewer(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "client.info")
	dst := filepath.Join(dir, "hub.info")
	writeFileAt(t, dst, `{"auth":{"accessToken":"tok-old"}}`, time.Now().Add(-time.Hour))
	writeFileAt(t, src, liveCredSample, time.Now())
	t.Setenv("AIHUB_WB_LIVE_CRED", src)
	t.Setenv("AIHUB_WB_LIVE_DEST", dst)

	c := NewCredentialManager(dst, "https://www.workbuddy.cn", "www.workbuddy.cn", "UA")
	s, _, err := c.session()
	if err != nil {
		t.Fatalf("读凭据失败：%v", err)
	}
	if s.Auth.AccessToken != "tok-new" {
		t.Fatalf("应同步到新 token，得到 %q", s.Auth.AccessToken)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != liveCredSample {
		t.Fatalf("副本内容未更新：%s", b)
	}
}

func TestSyncDomLiveCredKeepsNewerCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "client.info")
	dst := filepath.Join(dir, "hub.info")
	writeFileAt(t, src, `{"auth":{"accessToken":"tok-old"}}`, time.Now().Add(-2*time.Hour))
	writeFileAt(t, dst, liveCredSample, time.Now())
	t.Setenv("AIHUB_WB_LIVE_CRED", src)
	t.Setenv("AIHUB_WB_LIVE_DEST", dst)

	c := NewCredentialManager(dst, "https://www.workbuddy.cn", "www.workbuddy.cn", "UA")
	if _, _, err := c.session(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != liveCredSample {
		t.Fatalf("副本比源新时不该被覆盖，得到 %s", b)
	}
}

func TestSyncDomLiveCredOnlyForDestPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "client.info")
	dst := filepath.Join(dir, "hub.info")
	other := filepath.Join(dir, "other.info")
	writeFileAt(t, src, liveCredSample, time.Now())
	writeFileAt(t, dst, `{"auth":{"accessToken":"tok-old"}}`, time.Now().Add(-time.Hour))
	writeFileAt(t, other, `{"auth":{"accessToken":"tok-old"}}`, time.Now().Add(-time.Hour))
	t.Setenv("AIHUB_WB_LIVE_CRED", src)
	t.Setenv("AIHUB_WB_LIVE_DEST", dst)

	c := NewCredentialManager(other, "https://www.workbuddy.cn", "www.workbuddy.cn", "UA")
	if _, _, err := c.session(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(other)
	if !strings.Contains(string(b), "tok-old") {
		t.Fatalf("非目标路径不该被同步：%s", b)
	}
}

func TestSyncDomLiveCredSkipsBrokenSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "client.info")
	dst := filepath.Join(dir, "hub.info")
	writeFileAt(t, src, "{ this is not json", time.Now())
	writeFileAt(t, dst, `{"auth":{"accessToken":"tok-old"}}`, time.Now().Add(-time.Hour))
	t.Setenv("AIHUB_WB_LIVE_CRED", src)
	t.Setenv("AIHUB_WB_LIVE_DEST", dst)

	c := NewCredentialManager(dst, "https://www.workbuddy.cn", "www.workbuddy.cn", "UA")
	if _, _, err := c.session(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if !strings.Contains(string(b), "tok-old") {
		t.Fatalf("源文件坏掉时不该覆盖副本：%s", b)
	}
}

func TestSyncDomLiveCredCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "client.info")
	dst := filepath.Join(dir, "hub.info")
	writeFileAt(t, src, liveCredSample, time.Now())
	writeFileAt(t, dst, `{"auth":{"accessToken":"tok-old"}}`, time.Now().Add(-time.Hour))
	t.Setenv("AIHUB_WB_LIVE_CRED", "off")
	t.Setenv("AIHUB_WB_LIVE_DEST", dst)

	c := NewCredentialManager(dst, "https://www.workbuddy.cn", "www.workbuddy.cn", "UA")
	if _, _, err := c.session(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dst)
	if !strings.Contains(string(b), "tok-old") {
		t.Fatalf("AIHUB_WB_LIVE_CRED=off 时应关闭同步：%s", b)
	}
}
