package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 每日积分快照（2026-10-12 用户要求）：
// 国际版活跃奖励「次日发放」，所以当天看不出效果。这里记每日余额，
// 直接显示「较前一日 +30」，不必去官网翻积分明细。

func resetCreditHistory(t *testing.T) {
	t.Helper()
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() {
		hubRoot = old
		creditHistoryState.mu.Lock()
		creditHistoryState.loaded = false
		creditHistoryState.entries = map[string]*creditHistoryEntry{}
		creditHistoryState.mu.Unlock()
	})
	creditHistoryState.mu.Lock()
	creditHistoryState.loaded = false
	creditHistoryState.entries = map[string]*creditHistoryEntry{}
	creditHistoryState.mu.Unlock()
}

// 同一账号同一天重复记录：只保留最后一条（当日最终余额），不重复写盘
func TestCreditHistorySameDayOverwrite(t *testing.T) {
	resetCreditHistory(t)
	creditHistoryRecord("uid-a", "甲", 100, 200)
	creditHistoryRecord("uid-a", "甲", 130, 200)

	got, ok := creditHistoryToday("uid-a")
	if !ok {
		t.Fatal("应有今天的记录")
	}
	if got.Remain != 130 {
		t.Fatalf("同日应覆盖为最后值 130，实际 %v", got.Remain)
	}
	// 文件里也只应有一条
	b, err := os.ReadFile(creditHistoryPath())
	if err != nil {
		t.Fatalf("应已落盘：%v", err)
	}
	if n := strings.Count(string(b), time.Now().Format("2006-01-02")); n != 1 {
		t.Fatalf("同一天应只有 1 条记录，实际 %d", n)
	}
}

// 按 uid 分桶，不按凭证目录名（目录名会变，uid 才稳定）
func TestCreditHistoryKeyedByUID(t *testing.T) {
	resetCreditHistory(t)
	creditHistoryRecord("uid-1", "甲", 10, 100)
	creditHistoryRecord("uid-2", "乙", 20, 100)

	a, _ := creditHistoryToday("uid-1")
	b, _ := creditHistoryToday("uid-2")
	if a.Remain != 10 || b.Remain != 20 {
		t.Fatalf("应按 uid 分开存：uid-1=%v uid-2=%v", a.Remain, b.Remain)
	}
	// 换目录名（同名账号）不该新建桶
	creditHistoryRecord("uid-1", "甲改名", 15, 100)
	a2, _ := creditHistoryToday("uid-1")
	if a2.Remain != 15 {
		t.Fatalf("同名账号应更新原桶：%v", a2.Remain)
	}
}

// 差额文案：昨天 100、今天 130 → 「较 10-xx +30」
func TestCreditHistoryDiffText(t *testing.T) {
	resetCreditHistory(t)
	// 手工塞入昨天的记录
	y := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	creditHistoryState.mu.Lock()
	creditHistoryState.loaded = true
	creditHistoryState.entries["uid-x"] = &creditHistoryEntry{
		Name: "甲",
		Days: map[string]creditSnapshot{
			y: {Remain: 100, Total: 200},
			time.Now().Format("2006-01-02"): {Remain: 130, Total: 200},
		},
	}
	creditHistoryState.mu.Unlock()

	got := creditHistoryDiffText("uid-x")
	if !strings.Contains(got, "+30") {
		t.Fatalf("应显示 +30，实际 %q", got)
	}
	if !strings.Contains(got, y[5:]) {
		t.Fatalf("应含对比日期 %s，实际 %q", y[5:], got)
	}
	t.Logf("差额文案：%s", got)
}

// 减少时显示负号；持平显示「持平」
func TestCreditHistoryDiffNegativeAndFlat(t *testing.T) {
	resetCreditHistory(t)
	y := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	today := time.Now().Format("2006-01-02")

	creditHistoryState.mu.Lock()
	creditHistoryState.loaded = true
	creditHistoryState.entries["uid-neg"] = &creditHistoryEntry{Days: map[string]creditSnapshot{
		y: {Remain: 100}, today: {Remain: 70},
	}}
	creditHistoryState.entries["uid-flat"] = &creditHistoryEntry{Days: map[string]creditSnapshot{
		y: {Remain: 100}, today: {Remain: 100},
	}}
	creditHistoryState.mu.Unlock()

	if got := creditHistoryDiffText("uid-neg"); !strings.Contains(got, "-30") {
		t.Fatalf("减少应显示 -30，实际 %q", got)
	}
	if got := creditHistoryDiffText("uid-flat"); !strings.Contains(got, "持平") {
		t.Fatalf("持平应显示「持平」，实际 %q", got)
	}
}

// 没有历史时不该编造差额
func TestCreditHistoryDiffNoPrev(t *testing.T) {
	resetCreditHistory(t)
	creditHistoryRecord("uid-new", "新", 50, 100)
	if got := creditHistoryDiffText("uid-new"); got != "" {
		t.Fatalf("只有今天一条记录时不该给差额，实际 %q", got)
	}
}

// uid 为空时跳过（没有身份就无法归属）
func TestCreditHistorySkipsEmptyUID(t *testing.T) {
	resetCreditHistory(t)
	creditHistoryRecord("", "无名", 1, 2)
	if _, ok := creditHistoryToday(""); ok {
		t.Fatal("uid 为空不该记录")
	}
}

// 落盘 + 重新加载：数据要能持久化（重启后仍能对比）
func TestCreditHistoryPersists(t *testing.T) {
	resetCreditHistory(t)
	creditHistoryRecord("uid-p", "甲", 42, 100)

	// 模拟重启：清内存、重新读盘
	creditHistoryState.mu.Lock()
	creditHistoryState.loaded = false
	creditHistoryState.entries = map[string]*creditHistoryEntry{}
	creditHistoryState.mu.Unlock()

	got, ok := creditHistoryToday("uid-p")
	if !ok || got.Remain != 42 {
		t.Fatalf("重启后应能读回：ok=%v %+v", ok, got)
	}
	if !fileExists(filepath.Join(hubRoot, creditHistoryFile)) {
		t.Fatalf("文件应存在：%s", creditHistoryPath())
	}
}

// 坏文件不该影响主流程
func TestCreditHistoryCorruptFile(t *testing.T) {
	resetCreditHistory(t)
	os.WriteFile(creditHistoryPath(), []byte(`{not json`), 0644)
	creditHistoryRecord("uid-c", "甲", 5, 10)
	if got, ok := creditHistoryToday("uid-c"); !ok || got.Remain != 5 {
		t.Fatalf("坏文件后应能正常记录：ok=%v %+v", ok, got)
	}
}
