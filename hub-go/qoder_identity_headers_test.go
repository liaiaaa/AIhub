package main

// 2026-11-03：qoder 出站身份与真桌面端对齐的回归测试。
// 背景（用户要求「做好隐藏」）：官网把来源显示成空白，通常是因为请求自报的
// 客户端身份和真客户端不一致。magpie 的 changelog 记了三条关键差异：
//   - Cosy-Version 是 worker runtime 版本（1.1.69），不是 1.0.0；
//   - chat 头里带 device identity 的 MachineToken 与 MachineType、以及 MachineHostname；
//   - 签名的 request id 是带连字符的 UUID，不是裸 hex。

import (
	"strings"
	"testing"
)

func TestEditUUIDStringDashed(t *testing.T) {
	b := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	got := editUUIDString(b)
	if len(got) != 36 || strings.Count(got, "-") != 4 {
		t.Fatalf("应为 8-4-4-4-12 的 UUID，实际 %q", got)
	}
	if got != "12345678-9abc-def0-1122-334455667788" {
		t.Errorf("UUID 格式不对：%q", got)
	}
	// 短输入不能 panic
	if s := editUUIDString([]byte{1, 2, 3}); len(s) != 36 {
		t.Errorf("短输入也应产出 36 字符，实际 %q", s)
	}
}

func TestQoderWorkerVersion(t *testing.T) {
	t.Setenv("QODER_WORKER_VERSION", "")
	if got := qoderWorkerVersion(); got != "1.1.69" {
		t.Errorf("默认 worker 版本应为 1.1.69，实际 %q", got)
	}
	t.Setenv("QODER_WORKER_VERSION", "9.9.9")
	if got := qoderWorkerVersion(); got != "9.9.9" {
		t.Errorf("env 应可覆盖，实际 %q", got)
	}
}

func TestQoderAppIdentitySendsDesktopFields(t *testing.T) {
	id := qoderIdentityPresets["app"]
	if id.ClientType != "10" {
		t.Errorf("桌面端 Cosy-ClientType 应为 10，实际 %q", id.ClientType)
	}
	if id.Product != "app" || id.SessionType != "app" || id.IDEPlatform != "app" {
		t.Errorf("app 预设的三个身份字段应一致：%+v", id)
	}
	if id.UserAgent != "Qoder" {
		t.Errorf("UA 应为 Qoder，实际 %q", id.UserAgent)
	}
	if id.CosyVersion != qoderWorkerVersion() {
		t.Errorf("Cosy-Version 应取 worker runtime 版本，实际 %q", id.CosyVersion)
	}
	if !id.SendMachineOS {
		t.Error("桌面端会发 Cosy-MachineOS")
	}
}
