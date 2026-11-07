package main

// 2026-11-07 Qoder 探测两个口径 bug 的回归防线：
//  1. qfmodel 官方 price_factor=0.1 但 is_free=true（残留）→ 必须显示 0.1x，不是免费；
//  2. QoDuck(lmodel) 只在 app 分组、price_factor=0 → 必须被合并进来且显示免费。
//
// 证据：.tmp/qoder-catalog-raw-self.json（官方 /algo/api/v2/model/list 原始响应）。

import (
	"testing"
)

func TestQoderEntryRatioPriceFactorWinsOverStaleIsFree(t *testing.T) {
	cases := []struct {
		name      string
		e         qoderModelEntry
		wantLabel string
		wantFree  bool
	}{
		{"qfmodel 0.1x 不是免费", qoderModelEntry{Key: "qfmodel", PriceFactor: 0.1, IsFree: true}, "0.1x", false},
		{"QoDuck 真免费", qoderModelEntry{Key: "lmodel", PriceFactor: 0, IsFree: true}, "免费", true},
		{"普通倍率", qoderModelEntry{Key: "gmodel", PriceFactor: 0.8}, "0.8x", false},
		{"无数据", qoderModelEntry{Key: "unknown", PriceFactor: 0}, "", false},
	}
	for _, c := range cases {
		label, free := qoderEntryRatio(c.e)
		if label != c.wantLabel || free != c.wantFree {
			t.Errorf("%s: qoderEntryRatio = (%q,%v), want (%q,%v)", c.name, label, free, c.wantLabel, c.wantFree)
		}
	}
}

// parseQoderModelList 必须把 app 分组独有的 QoDuck 并进主目录（按 key 去重）。
func TestParseQoderModelListMergesAppGroup(t *testing.T) {
	raw := []byte(`{
		"chat": [
			{"key":"qfmodel","display_name":"Qwen3.8-Flash","enable":true,"price_factor":0.1,"is_free":true}
		],
		"app": [
			{"key":"qfmodel","display_name":"Qwen3.8-Flash","enable":true,"price_factor":0.1,"is_free":true},
			{"key":"lmodel","display_name":"QoDuck","enable":true,"price_factor":0,"is_free":true,"tags":["free"]}
		],
		"byok_teams": [
			{"key":"should-be-ignored","display_name":"BYOK","enable":true}
		],
		"experts": [
			{"key":"should-also-be-ignored","display_name":"Expert","enable":true}
		]
	}`)
	entries, err := parseQoderModelList(raw)
	if err != nil {
		t.Fatalf("parseQoderModelList: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("条目数 = %d, want 2（chat 1 + app 独有 1）: %+v", len(entries), entries)
	}
	byKey := map[string]qoderModelEntry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	qf, ok := byKey["qfmodel"]
	if !ok {
		t.Fatal("主分组 qfmodel 丢失")
	}
	if label, free := qoderEntryRatio(qf); label != "0.1x" || free {
		t.Errorf("qfmodel = (%q,%v), want (0.1x,false)", label, free)
	}
	lm, ok := byKey["lmodel"]
	if !ok {
		t.Fatal("app 分组独有的 QoDuck 未被合并（这正是用户报的 bug）")
	}
	if label, free := qoderEntryRatio(lm); label != "免费" || !free {
		t.Errorf("lmodel = (%q,%v), want (免费,true)", label, free)
	}
	for _, e := range entries {
		if e.Key == "should-be-ignored" || e.Key == "should-also-be-ignored" {
			t.Errorf("不应引入非补充分组的条目 %q", e.Key)
		}
	}
}

// 内置离线快照同样要带 QoDuck 且 qfmodel 是 0.1x（线路未启动时探测靠它）。
func TestDefaultQoderCatalogHasQoDuckAndFlashRatio(t *testing.T) {
	byKey := map[string]qoderModelEntry{}
	for _, e := range defaultQoderCatalog() {
		byKey[e.Key] = e
	}
	lm, ok := byKey["lmodel"]
	if !ok {
		t.Fatal("内置快照缺少 QoDuck(lmodel)")
	}
	if label, free := qoderEntryRatio(lm); label != "免费" || !free {
		t.Errorf("内置 QoDuck = (%q,%v), want (免费,true)", label, free)
	}
	qf := byKey["qfmodel"]
	if label, free := qoderEntryRatio(qf); label != "0.1x" || free {
		t.Errorf("内置 qfmodel = (%q,%v), want (0.1x,false)", label, free)
	}
	// qmodel_38max 官方 price_factor=0.5，不该带残留 IsFree
	if byKey["qmodel_38max"].IsFree {
		t.Error("qmodel_38max 官方不是免费，内置快照不该残留 IsFree=true")
	}
}
