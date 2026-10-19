package main

// 模型级限流记录的持久化。
//
// 用户 2026-10-17 报的现象：某个国际凭证跑出「deepseek-v4.1-flash 限流到明早 8 点」
// 的黄标，重启线路或重开 aihub 后标就没了，非得再撞一次限流才复现。
//
// 根因是载入判据用错了字段：rateLimitRecord 把 Until 按 rateLimitMaxUntil（2h）
// 截断，而 loadRateLimitLocked 只看 Until —— 于是「上游承诺 19 小时后恢复」的记录
// 在 2 小时后就不载入，下一次落盘（saveRateLimitLocked 只写内存里的条目）把它从文件
// 里彻底抹掉，ResetAt 那个真时刻跟着一起丢。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 写一份台账文件：模型级条目本地闸门已过、上游恢复时刻未到。
func writeLedger(t *testing.T, entries map[string]*rateLimitEntry) {
	t.Helper()
	b, err := json.Marshal(struct {
		Entries map[string]*rateLimitEntry `json:"entries"`
	}{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rateLimitPath(), b, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestModelLimitSurvivesWhileUpstreamPromiseIsLive(t *testing.T) {
	resetRateLimit(t)
	now := time.Now()
	writeLedger(t, map[string]*rateLimitEntry{
		rlKey("uid-A", rlKindModel, "deepseek-v4.1-flash"): {
			UID: "uid-A", Name: "<nick-A>", Kind: rlKindModel, Model: "deepseek-v4.1-flash",
			Code: 6004, Reason: "用量限流(6004)",
			Until:   now.Add(-1 * time.Hour), // 本地 2h 闸门已过
			ResetAt: now.Add(10 * time.Hour), // 上游说明显还在后面
			SeenAt:  now.Add(-3 * time.Hour), Hits: 3,
		},
	})

	limits := modelLimitsFor("uid-A")
	if len(limits) != 1 {
		t.Fatalf("上游承诺的恢复时刻还没到，这条限流不该消失（重启即复现的 bug）：%+v", limits)
	}
	if limits[0].Model != "deepseek-v4.1-flash" || !limits[0].UpstreamReset {
		t.Fatalf("条目内容不对：%+v", limits[0])
	}
	if limits[0].UpstreamUntil == "" {
		t.Error("该把上游的恢复时刻显示出来")
	}
}

// 两个时刻都过了 —— 该干净地丢掉，不留僵尸条目。
func TestModelLimitDroppedWhenBothTimesPassed(t *testing.T) {
	resetRateLimit(t)
	now := time.Now()
	writeLedger(t, map[string]*rateLimitEntry{
		rlKey("uid-B", rlKindModel, "glm-5.2"): {
			UID: "uid-B", Kind: rlKindModel, Model: "glm-5.2", Code: 6004,
			Until: now.Add(-2 * time.Hour), ResetAt: now.Add(-30 * time.Minute), SeenAt: now.Add(-4 * time.Hour),
		},
	})
	if l := modelLimitsFor("uid-B"); len(l) != 0 {
		t.Fatalf("上游时刻也过了，不该再显示：%+v", l)
	}
}

// 关键边界：**显示**延长到上游时刻，绝不等于**放行判定**也跟着延长。
// 上游给的恢复时刻本身不可信（2026-10-16 实测：台账写着限到 10-17 08:00，
// 当天 12:43 同一模型同一凭证就正常返回了）。所以 rateLimitFor 仍只看 Until，
// 到点就允许重试，不能被一个可能错误的远期时刻锁死账号。
func TestDisplayExtensionDoesNotExtendGating(t *testing.T) {
	resetRateLimit(t)
	now := time.Now()
	writeLedger(t, map[string]*rateLimitEntry{
		rlKey("uid-C", rlKindModel, "hy4-preview"): {
			UID: "uid-C", Kind: rlKindModel, Model: "hy4-preview", Code: 6004,
			Until: now.Add(-1 * time.Hour), ResetAt: now.Add(10 * time.Hour), SeenAt: now.Add(-3 * time.Hour),
		},
		rlKey("uid-D", rlKindAccount, ""): {
			UID: "uid-D", Kind: rlKindAccount, Code: 6004,
			Until: now.Add(-1 * time.Hour), ResetAt: now.Add(10 * time.Hour), SeenAt: now.Add(-3 * time.Hour),
		},
	})
	if _, ok := rateLimitFor("uid-C", "hy4-preview"); ok {
		t.Error("本地闸门已过就必须放行重试，不能拿上游的远期时刻继续拦")
	}
	if _, ok := rateLimitFor("uid-D", ""); ok {
		t.Error("账号级同理：显示可以长，拦不能长")
	}
	// 但记录本身要还在（否则控制台无从显示，也就回到用户报的那个 bug）。
	if l := modelLimitsFor("uid-C"); len(l) != 1 {
		t.Errorf("记录被过早清除：%+v", l)
	}
}

// 落盘再读回，必须还在 —— 用户说的「重启后消失」就是这一步丢的。
func TestModelLimitPersistedAcrossReload(t *testing.T) {
	resetRateLimit(t)
	now := time.Now()
	rateLimitRecord("uid-E", "buxiangxie", rlKindModel, "step-5-preview", 6004,
		"用量限流(6004)", now.Add(19*time.Hour))

	// 模拟进程重启：清空内存、强制重新载入。
	rateLimitState.mu.Lock()
	rateLimitState.loaded = false
	rateLimitState.entries = map[string]*rateLimitEntry{}
	rateLimitState.mu.Unlock()

	if _, err := os.Stat(filepath.Join(hubRoot, rateLimitFile)); err != nil {
		t.Fatalf("记录没落盘：%v", err)
	}
	limits := modelLimitsFor("uid-E")
	if len(limits) != 1 {
		t.Fatalf("重启后模型级限流记录丢失（用户报的现象）：%+v", limits)
	}
	if limits[0].Model != "step-5-preview" {
		t.Errorf("模型名不对：%+v", limits[0])
	}
}
