package main

// 2026-11-02：TeleAgent 线路的签名与模型清单锁定。
//
// 这套头是逆向得来的：super agent 安全头 + HMAC-SHA256 签名，上游真验证
// （缺头 40101 / 缺版本 40104 / 签名错 40104 invalid signature）。
// 这里锁住「算法形状」，一旦有人改动导致签名不匹配，测试先红。

import (
	"strings"
	"testing"
)

const taTestToken = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlLXNlZ21lbnQ"

func TestTeleagentSignHeadersShape(t *testing.T) {
	body := []byte(`{"model":"chat-flash","messages":[]}`)
	ts, nonce := "1793600000", "11111111-2222-4333-8444-555555555555"
	h, err := teleagentSignHeadersAt(taTestToken, "aabbccddeeff", "install-uuid", "POST",
		teleagentChatPath, "3.0.0-rc.1", body, ts, nonce)
	if err != nil {
		t.Fatalf("签名失败：%v", err)
	}
	// 必需头一个都不能少
	for _, k := range []string{
		"X-Token", "X-Route-Target", "X-App-Version", "X-SuperAgent-Timestamp",
		"X-SuperAgent-Nonce", "X-SuperAgent-Device-Id", "X-SuperAgent-Install-Id",
		"X-SuperAgent-Sign-Version", "X-SuperAgent-Signature",
	} {
		if h[k] == "" {
			t.Errorf("缺头 %s", k)
		}
	}
	if h["X-SuperAgent-Sign-Version"] != "v1" {
		t.Errorf("签名版本应为 v1，实际 %q", h["X-SuperAgent-Sign-Version"])
	}
	if len(h["X-SuperAgent-Signature"]) != 64 {
		t.Errorf("签名应是 64 位 hex，实际 %d 位", len(h["X-SuperAgent-Signature"]))
	}
	if strings.ToLower(h["X-SuperAgent-Signature"]) != h["X-SuperAgent-Signature"] {
		t.Error("签名应是小写 hex")
	}
	// 上游要求的固定路由头
	if h["X-Route-Target"] != "ops-gateway" {
		t.Errorf("X-Route-Target 应为 ops-gateway，实际 %q", h["X-Route-Target"])
	}
}

func TestTeleagentSignatureDependsOnEveryInput(t *testing.T) {
	body := []byte(`{"a":1}`)
	base, err := teleagentSignHeadersAt(taTestToken, "dev", "ins", "POST", teleagentChatPath, "3.0.0-rc.1", body, "1", "n")
	if err != nil {
		t.Fatal(err)
	}
	sig := func(tok, dev, ins, path, ver string, b []byte, ts, nonce string) string {
		h, err := teleagentSignHeadersAt(tok, dev, ins, "POST", path, ver, b, ts, nonce)
		if err != nil {
			t.Fatalf("签名失败：%v", err)
		}
		return h["X-SuperAgent-Signature"]
	}
	cases := map[string]string{
		"换 body":  sig(taTestToken, "dev", "ins", teleagentChatPath, "3.0.0-rc.1", []byte(`{"a":2}`), "1", "n"),
		"换 ts":    sig(taTestToken, "dev", "ins", teleagentChatPath, "3.0.0-rc.1", body, "2", "n"),
		"换 nonce": sig(taTestToken, "dev", "ins", teleagentChatPath, "3.0.0-rc.1", body, "1", "m"),
		"换 ins":   sig(taTestToken, "dev", "ins2", teleagentChatPath, "3.0.0-rc.1", body, "1", "n"),
		"换 ver":   sig(taTestToken, "dev", "ins", teleagentChatPath, "3.0.0", body, "1", "n"),
		"换 path":  sig(taTestToken, "dev", "ins", "/other", "3.0.0-rc.1", body, "1", "n"),
	}
	for name, got := range cases {
		if got == base["X-SuperAgent-Signature"] {
			t.Errorf("%s 后签名不应相同（说明该字段没参与签名）", name)
		}
	}
	// deviceId 只作为头发出、不参与签名材料（与上游实现一致：derivedKey 用的是
	// token/installId/ts/nonce）。这条断言把该事实钉住，免得以后误加进去导致签名失效。
	if sig(taTestToken, "another-device", "ins", teleagentChatPath, "3.0.0-rc.1", body, "1", "n") != base["X-SuperAgent-Signature"] {
		t.Error("deviceId 不应参与签名材料")
	}
	// 相同输入必须稳定
	if sig(taTestToken, "dev", "ins", teleagentChatPath, "3.0.0-rc.1", body, "1", "n") != base["X-SuperAgent-Signature"] {
		t.Error("相同输入应得到相同签名")
	}
}

func TestTeleagentRejectsBadToken(t *testing.T) {
	if _, err := teleagentSignHeadersAt("not-a-jwt", "d", "i", "POST", teleagentChatPath, "3.0.0-rc.1", []byte("{}"), "1", "n"); err == nil {
		t.Error("非 JWT 应报错")
	}
}

func TestTeleagentModelsExcludeDisabled(t *testing.T) {
	p := NewTeleAgentProxy(TeleAgentPort)
	if p.Port != 8794 {
		t.Errorf("端口应为 8794，实际 %d", p.Port)
	}
	ids := teleagentAdapter{h: &Hub{}}.Models()
	// 2026-11-03 用户指正「teleagent 应该就三个模型才对」：前台目录（model-catalog.json）
	// 只有 智能/旗舰/极速 三个，chat-nano 只在 TeleAgent.jsonc 的 provider.limits 里 → 隐藏。
	// 例外：**chat-lite 是桌面端的识图小模型**（runtime.js 的 smallModelRef = NewApi/chat-lite），
	// 图片识别全靠它，所以从隐藏放出，对外是 4 个。
	if len(ids) != 4 {
		t.Fatalf("对外模型应为 4 个（前台三个 + 识图用的 chat-lite），实际 %d：%v", len(ids), ids)
	}
	want := map[string]bool{"chat-pro": true, "chat-flagship": true, "chat-flash": true, "chat-lite": true}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("对外目录出现非前台模型 %q", id)
		}
		if id == "tele-safety-answer" {
			t.Error("tele-safety-answer 在应用里是 blacklist，不该对外暴露")
		}
	}
	// 隐藏 ≠ 删除：chat-nano 仍带 Hidden 标记（它是纯文本模型，前台不展示）；
	// chat-lite 已放出（识图模型），不再要求 Hidden。
	for _, id := range []string{"chat-nano"} {
		var found *bool
		for i := range teleagentModels {
			if teleagentModels[i].ID == id {
				f := teleagentModels[i].Hidden
				found = &f
			}
		}
		if found == nil {
			t.Errorf("%s 不在表里（隐藏应是 Hidden 标记，不是从表里删）", id)
		} else if !*found {
			t.Errorf("%s 应标记 Hidden（前台不展示）", id)
		}
	}
	// chat-lite 必须对外可见：它是图片识别用的那条
	{
		var liteHidden *bool
		for i := range teleagentModels {
			if teleagentModels[i].ID == "chat-lite" {
				v := teleagentModels[i].Hidden
				liteHidden = &v
			}
		}
		if liteHidden != nil && *liteHidden {
			t.Error("chat-lite 是识图模型，必须对外可见（Hidden 应为 false）")
		}
	}
	if !p.Running() == false {
		t.Error("新建线路不该是运行态")
	}
}
