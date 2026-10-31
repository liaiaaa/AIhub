package main

// Qoder 上游闸门的配置化行为。
//
// 2026-10-28 用户要求撤掉本地并发限制（「别设置本地并发限制了，太妨碍我的使用了」）：
// 默认 0 = 不限制，想恢复用 QODER_MAX_CONCURRENT=N 或数据根 qoder_max_concurrent.txt。
// 拟人间隔同理（QODER_MIN_GAP_MS / qoder_min_gap.txt，0 = 不等待）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 默认不限制：连抢 20 个槽位全部成功——多开会话/并行工具调用不再吃本地 429。
func TestQoderConcurrencyUnlimitedByDefault(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	t.Setenv(qoderMaxConcurrentEnv, "")

	if got := qoderMaxConcurrent(); got != 0 {
		t.Fatalf("默认上限应为 0（不限制），实际 %d", got)
	}

	p := NewQoderProxy()
	releases := make([]func(), 0, 20)
	for i := 0; i < 20; i++ {
		rel, ok := p.acquireUpstreamSlot()
		if !ok {
			t.Fatalf("第 %d 次抢槽被拒——默认不该有限制", i+1)
		}
		releases = append(releases, rel)
	}
	if got := p.inflightCount(); got != 20 {
		t.Errorf("在途计数 = %d, want 20", got)
	}
	for _, rel := range releases {
		rel()
	}
	if got := p.inflightCount(); got != 0 {
		t.Errorf("释放后计数 = %d, want 0", got)
	}
}

// 显式配置上限时仍然生效（池满 → 调用方 429）。
func TestQoderConcurrencyEnvCap(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	t.Setenv(qoderMaxConcurrentEnv, "2")

	if got := qoderMaxConcurrent(); got != 2 {
		t.Fatalf("上限 = %d, want 2", got)
	}
	p := NewQoderProxy()
	rel1, ok1 := p.acquireUpstreamSlot()
	rel2, ok2 := p.acquireUpstreamSlot()
	if !ok1 || !ok2 {
		t.Fatal("前两个应抢到")
	}
	if _, ok3 := p.acquireUpstreamSlot(); ok3 {
		t.Error("第 3 个应被拒（上限 2）")
	}
	rel1()
	if _, ok3 := p.acquireUpstreamSlot(); !ok3 {
		t.Error("释放一个后应能再抢到")
	}
	rel2()
}

// 数据根文件同样能配；环境变量优先；超封顶按封顶算；非法值回落默认。
func TestQoderConcurrencyFileEnvAndClamp(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	t.Setenv(qoderMaxConcurrentEnv, "")

	setFile := func(v string) {
		if err := os.WriteFile(filepath.Join(hubRoot, qoderMaxConcurrentFile), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	setFile("3")
	if got := qoderMaxConcurrent(); got != 3 {
		t.Errorf("文件配置 3 → %d", got)
	}
	t.Setenv(qoderMaxConcurrentEnv, "5")
	if got := qoderMaxConcurrent(); got != 5 {
		t.Errorf("环境变量应优先于文件：%d, want 5", got)
	}
	t.Setenv(qoderMaxConcurrentEnv, "")
	setFile("9999")
	if got := qoderMaxConcurrent(); got != qoderMaxConcurrentCeil {
		t.Errorf("超封顶应截到 %d，实际 %d", qoderMaxConcurrentCeil, got)
	}
	setFile("abc")
	if got := qoderMaxConcurrent(); got != qoderMaxConcurrentDefault {
		t.Errorf("非法值应回落默认 %d，实际 %d", qoderMaxConcurrentDefault, got)
	}
}

// 间隔配 0 时直接放行：不抢锁、不等待。
func TestQoderMinGapZeroSkipsWait(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	t.Setenv(qoderMinGapEnv, "0")

	if got := qoderMinGap(); got != 0 {
		t.Fatalf("间隔应为 0，实际 %v", got)
	}
	p := NewQoderProxy()
	// 连续两次：若仍按默认 800ms 排队，第二次要等 ~0.8-1.2s
	start := time.Now()
	for i := 0; i < 2; i++ {
		if !p.waitHumanGap(context.Background()) {
			t.Fatal("waitHumanGap 返回 false")
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("间隔为 0 时不该等待，实际耗时 %v", d)
	}
}

// 间隔仍可按需配置回来（默认 800ms）。
func TestQoderMinGapDefaultAndEnv(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	t.Setenv(qoderMinGapEnv, "")
	if got := qoderMinGap(); got != 800*time.Millisecond {
		t.Errorf("默认间隔 = %v, want 800ms", got)
	}
	t.Setenv(qoderMinGapEnv, "150")
	if got := qoderMinGap(); got != 150*time.Millisecond {
		t.Errorf("环境变量配置 = %v, want 150ms", got)
	}
	t.Setenv(qoderMinGapEnv, "-5")
	if got := qoderMinGap(); got != 800*time.Millisecond {
		t.Errorf("负数应回落默认，实际 %v", got)
	}
	t.Setenv(qoderMinGapEnv, "999999")
	if got := qoderMinGap(); got != time.Duration(qoderMinGapCeil)*time.Millisecond {
		t.Errorf("超封顶应截到 %dms，实际 %v", qoderMinGapCeil, got)
	}
}

// release 必须幂等：重复调用不能把在途计数减穿（否则并发闸门会被慢慢"放宽"）。
func TestQoderReleaseIdempotent(t *testing.T) {
	p := NewQoderProxy()
	rel, ok := p.acquireUpstreamSlot()
	if !ok {
		t.Fatal("抢槽失败")
	}
	rel()
	rel()
	rel()
	if got := p.inflightCount(); got != 0 {
		t.Errorf("重复释放后计数 = %d, want 0", got)
	}
}
