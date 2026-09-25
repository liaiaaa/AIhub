package main

// 针对**真实 Qoder 网关**的集成测试，默认跳过（不联网、不依赖凭据）。
//
// 为什么单独放一个文件：动态目录拉取是本模块唯一"签名 + 真实网络"的新代码路径，
// 单测里的 httptest 无法验证 COSY 签名对 GET（空 body）是否正确。
// 需要手动确证时执行：
//
//	$env:QODER_LIVE_TEST="1"; go test -run TestLiveFetchQoderCatalog -v ./...
//
// 该测试只读取桌面端凭据并调用模型目录接口，不发送任何推理请求。

import (
	"os"
	"testing"
	"time"
)

func TestLiveFetchQoderCatalog(t *testing.T) {
	if os.Getenv("QODER_LIVE_TEST") == "" {
		t.Skip("跳过实时集成测试；设置 QODER_LIVE_TEST=1 以启用")
	}

	auth, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	if err := auth.Reload(); err != nil {
		t.Fatalf("读取桌面端凭据失败（请确认 Qoder CN 桌面端已登录）：%v", err)
	}

	client := directClient()
	client.Timeout = 30 * time.Second

	entries, err := fetchQoderCatalog(t.Context(), auth, client, QoderModelListURL)
	if err != nil {
		t.Fatalf("fetchQoderCatalog: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("官方目录返回 0 个模型")
	}

	t.Logf("官方目录返回 %d 个模型：", len(entries))
	byKey := map[string]qoderModelEntry{}
	for _, e := range entries {
		byKey[e.Key] = e
		t.Logf("  %-15s %-22s vendor=%-9s reasoning=%-5v vision=%-5v free=%v",
			e.Key, e.DisplayName, e.Vendor, e.IsReasoning, e.IsVision, e.IsFree)
	}

	// 用户明确关心的 GLM / DeepSeek 系列必须存在
	for _, key := range []string{"gmodel", "gfmodel", "gm51model", "dmodel", "dfmodel", "qfmodel"} {
		if _, ok := byKey[key]; !ok {
			t.Errorf("官方目录缺少 %q", key)
		}
	}

	// 目录必须能构建出可用的别名表（显示名 → key）
	aliases := buildQoderAliases(entries)
	for display, wantKey := range map[string]string{
		"GLM-5.3":         "gmodel",
		"DeepSeek-V4-Pro": "dmodel",
		"Qwen3.8-Flash":   "qfmodel",
	} {
		if got := aliases[qoderNormalizeAlias(display)]; got != wantKey {
			t.Errorf("别名 %q → %q, want %q", display, got, wantKey)
		}
	}

	// 默认 key 必须落在目录内
	def := qoderDefaultKey(entries)
	if _, ok := byKey[def]; !ok {
		t.Errorf("默认 key %q 不在官方目录内", def)
	}
	t.Logf("解析出的默认模型：%s", def)
}
