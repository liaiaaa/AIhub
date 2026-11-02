package main

// 2026-11-02：Qoder 额度汇总口径。
//
// 用户报「qoder 的积分统计有误，现在有赠送积分 2000 你没统计」。
// 用真实返回（curl /sash/api/v2/me/usage，2026-11-02 实测）核对：
//
//	userQuota    remaining=1316 total=2000   ← 订阅额度
//	addOnQuota   remaining=1100 total=2900   ← 加油包
//	dedicatedResourcePackages[0]
//	    name=act-20260901-170 remaining=2000 total=2000 used=0
//	    status=QUOTA_DETAIL_STATUS_ACTIVE expiresAt=1795063856033
//	    displayLabels[title].valueI18n["zh-CN"]="Qwen 专属积分"
//
// 原实现只累加 userQuota + addOnQuota = 2416，漏了活动赠送的 2000。

import (
	"encoding/json"
	"strings"
	"testing"
)

const qoderUsageSample = `{
 "displayMode": "qoder",
 "qoderUsage": {
  "addOnQuota": {"detailUrl":"https://qoder.cn/account/usage","percentage":0.62,
                 "remaining":1100,"total":2900,"unit":"credits","used":1800},
  "dedicatedResourcePackages": [
   {"available":true,"description":"growth-campaign:act-20260901-170:grant:01a157a5",
    "displayLabels":[{"dimension":"title","value":"qwen model series",
       "valueI18n":{"en-US":"Qwen Exclusive Credits","zh-CN":"Qwen 专属积分"}}],
    "expiresAt":1795063856033,"id":"01a157a5-f7d5-7a51-aa79-32db47b413f9",
    "name":"act-20260901-170","percentage":0,"remaining":2000,
    "status":"QUOTA_DETAIL_STATUS_ACTIVE","total":2000,"unit":"credits","used":0}
  ],
  "expiresAt": 1795190400000,
  "isQuotaExceeded": false,
  "userQuota": {"percentage":0.34,"remaining":1316,"total":2000,"unit":"credits","used":684},
  "userType": "personal_professional"
 }
}`

func parseQoderUsageSample(t *testing.T) *qoderUsagePayload {
	t.Helper()
	var env struct {
		QoderUsage *qoderUsagePayload `json:"qoderUsage"`
	}
	if err := json.Unmarshal([]byte(qoderUsageSample), &env); err != nil {
		t.Fatalf("样本解析失败：%v", err)
	}
	if env.QoderUsage == nil {
		t.Fatal("样本里没有 qoderUsage")
	}
	return env.QoderUsage
}

func TestQoderCreditsIncludesGiftedPackage(t *testing.T) {
	usage := parseQoderUsageSample(t)
	info := qoderCreditsFromUsage(usage)
	if !info.OK {
		t.Fatalf("应成功：%+v", info)
	}
	// 主额度 = 1316 + 1100 = 2416；加上赠送 2000 = 4416；总额 2000+2900+2000 = 6900
	// 注意 fmtCN 会加千分位
	for _, want := range []string{"4,416", "6,900", "主额度 2,416", "Qwen 专属积分 2,000"} {
		if !strings.Contains(info.Text, want) {
			t.Errorf("文案缺少 %q：%s", want, info.Text)
		}
	}
}

func TestQoderCreditsSkipsInactivePackages(t *testing.T) {
	usage := parseQoderUsageSample(t)
	// 状态不是 ACTIVE（如已过期/已耗尽）的包不计入
	usage.DedicatedResourcePackages[0].Status = "QUOTA_DETAIL_STATUS_EXPIRED"
	info := qoderCreditsFromUsage(usage)
	if strings.Contains(info.Text, "Qwen 专属积分") {
		t.Errorf("过期包不该出现：%s", info.Text)
	}
	if !strings.Contains(info.Text, "2,416") {
		t.Errorf("过期后应回到主额度 2416：%s", info.Text)
	}
}

func TestQoderCreditsNoGiftText(t *testing.T) {
	// 没有赠送包时，文案保持老样式（不出现「另含」）
	usage := &qoderUsagePayload{}
	usage.UserQuota = qoderUsageQuota{Remaining: 100, Total: 200}
	info := qoderCreditsFromUsage(usage)
	if strings.Contains(info.Text, "另含") {
		t.Errorf("无赠送包时不该有「另含」：%s", info.Text)
	}
	if !strings.Contains(info.Text, "100") || !strings.Contains(info.Text, "200") {
		t.Errorf("主额度显示不对：%s", info.Text)
	}
}
