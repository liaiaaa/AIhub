package main

// Qoder 出站身份：预设常量、切换优先级、版本探测、签名头。
// 这些值全部来自本机逆向（桌面端 app-extract / CLI worker-runtime），
// 改动等于换身份，必须有测试盯住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQoderIdentityPresets(t *testing.T) {
	app := qoderIdentityPresets["app"]
	if app.ClientType != "10" || app.IDEPlatform != "app" || app.Product != "app" || app.SessionType != "app" {
		t.Errorf("app 预设与官方常量不符：%+v", app)
	}
	if app.UserAgent != "Qoder" || app.Accept != "text/event-stream, application/json" || app.AcceptEnc != "identity" {
		t.Errorf("app 预设头字段与官方不符：%+v", app)
	}
	if app.SendHostname {
		t.Error("app 预设不应发 hostname（官方桌面端不发）")
	}

	cli := qoderIdentityPresets["cli"]
	if cli.ClientType != "5" || cli.IDEPlatform != "qoder_cli" || cli.Product != "cli" || cli.SessionType != "qoderclicn" {
		t.Errorf("cli 预设与官方常量不符：%+v", cli)
	}
	if !cli.SendHostname || cli.AcceptLang != "en-US" {
		t.Errorf("cli 预设头字段与官方不符：%+v", cli)
	}
}

func TestQoderIdentitySwitch(t *testing.T) {
	oldRoot := hubRoot
	t.Cleanup(func() { hubRoot = oldRoot })
	hubRoot = t.TempDir()

	// 环境变量 > 文件 > 默认
	t.Setenv(qoderIdentityEnv, "")
	if got := qoderIdentityName(); got != "app" {
		t.Errorf("无任何配置应默认 app，实际 %q", got)
	}
	if err := os.WriteFile(dataPath(qoderIdentityFile), []byte("cli\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := qoderIdentityName(); got != "cli" {
		t.Errorf("文件应为 cli，实际 %q", got)
	}
	t.Setenv(qoderIdentityEnv, "app")
	if got := qoderIdentityName(); got != "app" {
		t.Errorf("环境变量应压过文件，实际 %q", got)
	}
	// 非法环境变量忽略，回落文件
	t.Setenv(qoderIdentityEnv, "bogus")
	if got := qoderIdentityName(); got != "cli" {
		t.Errorf("非法环境变量应忽略并回落文件 cli，实际 %q", got)
	}
	// 非法文件也忽略，回落默认
	if err := os.WriteFile(dataPath(qoderIdentityFile), []byte("weird"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(qoderIdentityEnv, "nope")
	if got := qoderIdentityName(); got != "app" {
		t.Errorf("非法文件应回落 app，实际 %q", got)
	}
}

func TestQoderVersionProbe(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".qoder-cn")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(dir, ".qoder-app-status.json")
	if err := os.WriteFile(statusPath, []byte(`{"version":"0.9.9","product":"qodercn"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := qoderProbeAppVersion(home); got != "0.9.9" {
		t.Errorf("应读出 0.9.9，实际 %q", got)
	}
	// product 不符不采信
	if err := os.WriteFile(statusPath, []byte(`{"version":"9.9.9","product":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := qoderProbeAppVersion(home); got != qoderAppVersionFallback {
		t.Errorf("product 不符应回落 %q，实际 %q", qoderAppVersionFallback, got)
	}
	// 文件不存在回落
	if got := qoderProbeAppVersion(t.TempDir()); got != qoderAppVersionFallback {
		t.Errorf("文件不存在应回落 %q，实际 %q", qoderAppVersionFallback, got)
	}
	// JSON 损坏回落
	if err := os.WriteFile(statusPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := qoderProbeAppVersion(home); got != qoderAppVersionFallback {
		t.Errorf("损坏 JSON 应回落 %q，实际 %q", qoderAppVersionFallback, got)
	}
}

func TestQoderSignIdentityHeaders(t *testing.T) {
	t.Setenv(qoderIdentityEnv, "app")
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	headers, err := a.Sign(QoderInferURL, `{"messages":[]}`)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	id := currentQoderIdentity()
	want := map[string]string{
		"Cosy-ClientType":       "10",
		"X-IDE-Platform":        "app",
		"X-Version":             id.Version,
		"Cosy-Version":          "1.0.0",
		"Cosy-Business-Product": "app",
		"User-Agent":            "Qoder",
		"Accept":                "text/event-stream, application/json",
		"Accept-Encoding":       "identity",
	}
	for k, v := range want {
		if headers[k] != v {
			t.Errorf("header %s = %q, want %q", k, headers[k], v)
		}
	}
	if !strings.Contains(headers["Cosy-MachineOS"], "_") {
		t.Errorf("Cosy-MachineOS 形态不对：%q", headers["Cosy-MachineOS"])
	}
	if headers["Cosy-MachineHostname"] != "" {
		t.Error("app 预设不应发 Cosy-MachineHostname")
	}
	if !strings.HasPrefix(headers["Authorization"], "Bearer COSY.") {
		t.Error("COSY 签名链被破坏了")
	}
}
