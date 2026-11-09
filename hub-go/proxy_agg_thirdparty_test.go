package main

import "testing"

// resolveAggThirdParty 必须与 resolveAggModel 在「第三方」范围内完全等价：
// 官方 gpt-* 由 resolveAggModel 兜底，但 resolveAggThirdParty 必须拒绝。
func TestResolveAggThirdPartyExcludesOfficial(t *testing.T) {
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "D"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	cases := []struct {
		slug string
		want bool
	}{
		{"dom.deepseek-v4-flash", true},     // 白名单精确命中
		{"qoder.DeepSeek-Flash", true},      // <线路>.<模型> 动态形式
		{"intl.some-new-model", true},       // 线路已注册即可
		{"gpt-6.1-sol", false},              // 官方前缀：第三方解析必须拒绝
		{"codex-auto-review", false},        // 非 gpt- 官方 slug 也不是第三方
		{"ccs-dom/deepseek-v4-flash", true}, // Hub Switch 时代写法
		{"totally-unknown", false},
	}
	for _, c := range cases {
		if _, ok := resolveAggThirdParty(c.slug); ok != c.want {
			t.Errorf("resolveAggThirdParty(%q) = %v，应为 %v", c.slug, ok, c.want)
		}
	}
	// 官方前缀在完整 resolveAggModel 里仍然命中（现有 /v1/* 行为不变）
	if am, ok := resolveAggModel("gpt-6.1-sol"); !ok || am.Line != "official" {
		t.Errorf("resolveAggModel(gpt-6.1-sol) = %+v, %v；官方兜底不能丢", am, ok)
	}
}

// 重构后 resolveAggModel 对第三方 slug 的解析结果必须逐字段不变。
func TestResolveAggModelUnchangedForThirdParty(t *testing.T) {
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.deepseek-v4-flash", Backend: "deepseek-v4-flash", Display: "D"}}
	aggMu.Unlock()
	t.Cleanup(func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() })

	for _, slug := range []string{"dom.deepseek-v4-flash", "qoder.DeepSeek-Flash", "ccs-dom/deepseek-v4-flash"} {
		a, aok := resolveAggThirdParty(slug)
		b, bok := resolveAggModel(slug)
		if aok != bok || a != b {
			t.Errorf("%q：第三方解析 %+v/%v ≠ 完整解析 %+v/%v", slug, a, aok, b, bok)
		}
	}
}
