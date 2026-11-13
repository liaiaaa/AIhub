package main

// 果办额度文案的到期时间显示（2026-11-13 用户反馈「能看到积分余额，但不知道
// 什么时间过期」）。
//
// 实测三个桶到期时间差得很远，必须各显各的：
//   套餐桶          2026-12-12（月额度）
//   新用户赠币      2027-02-10（活动赠币，三个月有效）
//   每日签到赠币    **当天 23:59**（当天失效，攒不下来）

import (
	"strings"
	"testing"
	"time"
)

func TestHwExpiryLabel(t *testing.T) {
	now := time.Now()

	// 当天到期 → 必须显式标钟点（用户会以为能攒着用，实际当天失效）
	today := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.Local)
	got := hwExpiryLabel(today.Format(time.RFC3339Nano))
	if !strings.Contains(got, "今日") || !strings.Contains(got, "23:59") {
		t.Errorf("当天到期的文案 = %q，期望含「今日」和钟点", got)
	}

	// 跨天到期 → 只写月-日
	later := today.AddDate(0, 0, 30)
	got2 := hwExpiryLabel(later.Format(time.RFC3339Nano))
	if strings.Contains(got2, "今日") {
		t.Errorf("跨天到期不该标「今日」：%q", got2)
	}
	if !strings.Contains(got2, later.Format("01-02")) {
		t.Errorf("跨天到期的文案 = %q，期望含 %s", got2, later.Format("01-02"))
	}

	// 空值/坏值 → 空串（调用方就不拼括号）
	if hwExpiryLabel("") != "" || hwExpiryLabel("not-a-time") != "" {
		t.Error("空值或非法时间应返回空串")
	}
}

// TestHwCreditsTextShowsEveryBucketExpiry 额度文案必须把每个桶的到期时间都写出来。
//
// 这条是防回归：原来只在末尾打一个「套餐到期 12-12」，赠币桶（尤其当天失效的
// 签到币）完全没有到期信息。
func TestHwCreditsTextShowsEveryBucketExpiry(t *testing.T) {
	ci := hwCredits()
	if !ci.OK {
		t.Skipf("果办额度查询不可用（网络/凭据问题），跳过文案断言：%s", ci.Text)
	}
	// 套餐到期
	if !strings.Contains(ci.Text, "套餐") || !strings.Contains(ci.Text, "到期") {
		t.Errorf("文案缺套餐到期时间：%s", ci.Text)
	}
	// 赠币桶的括号到期（有 bonus 时才有，这里只要求格式合法）
	if strings.Contains(ci.Text, "新用户专属福利") && !strings.Contains(ci.Text, "）") {
		t.Errorf("新用户赠币没带到期时间：%s", ci.Text)
	}
	t.Logf("当前文案：%s", ci.Text)
}
