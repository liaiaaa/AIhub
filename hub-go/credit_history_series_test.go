package main

// 2026-11-02：积分消耗统计（/api/credits/history）的算法锁定。
//
// 上游没有消费明细接口，消耗靠余额差反推：
//
//	Granted  = max(0, total - prevTotal)
//	Consumed = Granted - (remain - prevRemain)
//
// 这里用两天的最小样本把三种情形钉死：纯消耗 / 纯到账 / 到账后又消耗。

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func writeCreditHistoryFixture(t *testing.T, accounts map[string]any) {
	t.Helper()
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	b, err := json.MarshalIndent(map[string]any{"accounts": accounts}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creditHistoryPath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	creditHistoryState.mu.Lock()
	creditHistoryState.loaded = false
	creditHistoryState.entries = map[string]*creditHistoryEntry{} // 不能置 nil：load 会往里写
	creditHistoryState.mu.Unlock()
}

func day(d time.Time) string { return d.Format("2006-01-02") }

func TestCreditHistorySeriesConsumption(t *testing.T) {
	yesterday := day(time.Now().AddDate(0, 0, -1))
	today := day(time.Now())
	writeCreditHistoryFixture(t, map[string]any{
		"uid-pure-use": map[string]any{
			"name": "纯消耗账号",
			"days": map[string]any{
				yesterday: map[string]any{"remain": 100.0, "total": 100.0},
				today:     map[string]any{"remain": 85.0, "total": 100.0},
			},
		},
		"uid-granted": map[string]any{
			"name": "到账账号",
			"days": map[string]any{
				yesterday: map[string]any{"remain": 50.0, "total": 50.0},
				today:     map[string]any{"remain": 70.0, "total": 70.0},
			},
		},
	})

	series := creditHistorySeries(7)
	if len(series) != 2 {
		t.Fatalf("应有 2 个账号，实际 %d", len(series))
	}
	if series[0].UID != "uid-pure-use" {
		t.Fatalf("消耗多的应排前面，实际 %s", series[0].UID)
	}
	pure := series[0]
	if pure.PeriodUsed != 15 {
		t.Errorf("纯消耗账号 7 天消耗应 15，实际 %v", pure.PeriodUsed)
	}
	if pure.TodayUsed != 15 {
		t.Errorf("今日消耗应 15，实际 %v", pure.TodayUsed)
	}
	if pure.PeriodGranted != 0 {
		t.Errorf("没有到账时 Granted 应为 0，实际 %v", pure.PeriodGranted)
	}
	granted := series[1]
	if granted.PeriodGranted != 20 {
		t.Errorf("到账账号 Granted 应 20，实际 %v", granted.PeriodGranted)
	}
	if granted.PeriodUsed != 0 {
		t.Errorf("纯到账不该算成消耗，实际 %v", granted.PeriodUsed)
	}
	if granted.Latest.Remain != 70 {
		t.Errorf("最新余额应 70，实际 %v", granted.Latest.Remain)
	}
}

func TestCreditHistorySeriesGrantThenUse(t *testing.T) {
	d2 := day(time.Now().AddDate(0, 0, -2))
	d1 := day(time.Now().AddDate(0, 0, -1))
	d0 := day(time.Now())
	writeCreditHistoryFixture(t, map[string]any{
		"uid-mix": map[string]any{
			"name": "先到账后消耗",
			"days": map[string]any{
				d2: map[string]any{"remain": 100.0, "total": 100.0},
				d1: map[string]any{"remain": 200.0, "total": 200.0}, // 到账 +100
				d0: map[string]any{"remain": 180.0, "total": 200.0}, // 花掉 20
			},
		},
	})
	series := creditHistorySeries(7)
	if len(series) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(series))
	}
	got := series[0]
	if got.PeriodGranted != 100 {
		t.Errorf("到账应 100，实际 %v", got.PeriodGranted)
	}
	if got.PeriodUsed != 20 {
		t.Errorf("消耗应 20（不能把到账的 100 算成消耗），实际 %v", got.PeriodUsed)
	}
	if got.TodayUsed != 20 {
		t.Errorf("今日消耗应 20，实际 %v", got.TodayUsed)
	}
	if len(got.Days) != 3 {
		t.Fatalf("应 3 天，实际 %d", len(got.Days))
	}
	// 第一天没有基线，消耗/到账都应为 0
	if got.Days[0].Consumed != 0 || got.Days[0].Granted != 0 {
		t.Errorf("首日不该有差值，实际 %+v", got.Days[0])
	}
}
