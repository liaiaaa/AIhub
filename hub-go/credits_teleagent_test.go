package main

// 2026-11-03：TeleAgent 积分文案与解析的回归。
// 端点实测见 credits_teleagent.go 注释；这里只测纯函数，不打网络。

import (
	"encoding/json"
	"strings"
	"testing"
)

const taPointsFixture = `{"code":200,"data":{"pointsBalance":9688.18,"userRole":"INDIVIDUAL",
 "groups":[{"typeName":"积分福利社","active":[
   {"totalQuota":9700,"usedQuota":11.82,"balance":9688.18,"effectiveEnd":"2026-11-30 23:59:59"},
   {"totalQuota":300,"usedQuota":0,"balance":300,"effectiveEnd":"2026-12-31 23:59:59"}]}]}}`

const taOverviewFixture = `{"code":200,"data":{"taskCount":2,"callCount":3,"pointsUsed":11.82},"message":"success"}`

func TestTeleagentCreditText(t *testing.T) {
	var p teleagentPointsPayload
	if err := json.Unmarshal([]byte(taPointsFixture), &p); err != nil {
		t.Fatalf("points 夹具解析失败：%v", err)
	}
	var o teleagentUsageOverview
	if err := json.Unmarshal([]byte(taOverviewFixture), &o); err != nil {
		t.Fatalf("overview 夹具解析失败：%v", err)
	}
	got := teleagentCreditText(p, &o)
	want := "余额 9688.18 积分 · 今日消耗 11.82（3 次调用） · 最近到期 11-30"
	// 空格规范：`fmt.Fprintf` 前导空格与连接符可能差一个，统一压缩后比较
	if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(want), " ") {
		t.Errorf("文案不符\n got=%q\nwant=%q", got, want)
	}
}

func TestTeleagentCreditTextWithoutOverview(t *testing.T) {
	var p teleagentPointsPayload
	_ = json.Unmarshal([]byte(taPointsFixture), &p)
	got := teleagentCreditText(p, nil)
	if !strings.Contains(got, "余额 9688.18 积分") {
		t.Errorf("没有余额：%q", got)
	}
	if strings.Contains(got, "今日消耗") {
		t.Errorf("没有消耗数据时不该写「今日消耗」：%q", got)
	}
}

func TestTeleagentEarlyExpiryPicksEarliest(t *testing.T) {
	var p teleagentPointsPayload
	_ = json.Unmarshal([]byte(taPointsFixture), &p)
	if got := teleagentEarliestExpiry(p); got != "11-30" {
		t.Errorf("应取最早到期 11-30，实际 %q", got)
	}
	// 格式非法 → 不猜，返回空串
	bad := teleagentPointsPayload{}
	bad.Data.Groups = append(bad.Data.Groups, struct {
		TypeName string `json:"typeName"`
		Active   []struct {
			TotalQuota   float64 `json:"totalQuota"`
			UsedQuota    float64 `json:"usedQuota"`
			Balance      float64 `json:"balance"`
			EffectiveEnd string  `json:"effectiveEnd"`
		} `json:"active"`
	}{Active: []struct {
		TotalQuota   float64 `json:"totalQuota"`
		UsedQuota    float64 `json:"usedQuota"`
		Balance      float64 `json:"balance"`
		EffectiveEnd string  `json:"effectiveEnd"`
	}{{EffectiveEnd: "not-a-date"}}})
	if got := teleagentEarliestExpiry(bad); got != "" {
		t.Errorf("非法日期应返回空串，实际 %q", got)
	}
}

func TestTeleagentNumFormat(t *testing.T) {
	cases := map[float64]string{9688.18: "9688.18", 3: "3", 0: "0", 11.8: "11.80"}
	for in, want := range cases {
		if got := teleagentNum(in); got != want {
			t.Errorf("teleagentNum(%v) = %q，期望 %q", in, got, want)
		}
	}
}
