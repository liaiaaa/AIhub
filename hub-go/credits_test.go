package main

// 额度解析测试：用真实抓包样本（2026-10-07 www.workbuddy.ai 响应，字段为
// PascalCase 字符串）验证 wbSummaryParse / wbResourceParse。

import "testing"

const wbSummarySample = `{"code":0,"msg":"OK","requestId":"2e0ac095-71fd-482a-a6bf-5b53112c0e91","data":{"Packages":[
 {"PackageCode":"TCACA_code_007_nzdH5h4Nl0","CycleTotalCapacity":"30","CycleRemainCapacity":"0","CycleUsedCapacity":"30","CapacityUnit":"credit"},
 {"PackageCode":"TCACA_code_035_ArVxJcGDsm","CycleTotalCapacity":"100","CycleRemainCapacity":"100","CycleUsedCapacity":"0","CapacityUnit":"credits"}
],"SubscriptionPackageCode":"","SubscriptionPackageName":"Free","IsPaidUser":false}}`

func TestWbSummaryParse(t *testing.T) {
	remain, total, ok := wbSummaryParse([]byte(wbSummarySample))
	if !ok {
		t.Fatalf("解析失败（字符串数值应可解析）")
	}
	if remain != 100 || total != 130 {
		t.Fatalf("汇总错误: remain=%v total=%v（期望 100/130）", remain, total)
	}
}

func TestWbResourceParse(t *testing.T) {
	body := `{"code":0,"msg":"OK","data":{"response":{"data":{"accounts":[
	 {"cycleCapacityRemain":"12.5","cycleCapacitySize":"100"},
	 {"capacityRemain":"30","capacitySize":"50"}
	]}}}}`
	remain, total, ok := wbResourceParse([]byte(body))
	if !ok {
		t.Fatalf("解析失败")
	}
	if remain != 42.5 || total != 150 {
		t.Fatalf("汇总错误: remain=%v total=%v（期望 42.5/150）", remain, total)
	}
}
