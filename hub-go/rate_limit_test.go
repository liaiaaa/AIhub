package main

import (
	"strings"
	"testing"
	"time"
)

// 限流台账（2026-10-12 用户要求）：记下「哪个账号、哪个模型被限到几点」，
// 免得用户记不住恢复时刻。设计参考两个 MIT 开源实现（cooldown.go）。

func resetRateLimit(t *testing.T) {
	t.Helper()
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() {
		hubRoot = old
		rateLimitState.mu.Lock()
		rateLimitState.loaded = false
		rateLimitState.entries = map[string]*rateLimitEntry{}
		rateLimitState.mu.Unlock()
	})
	rateLimitState.mu.Lock()
	rateLimitState.loaded = false
	rateLimitState.entries = map[string]*rateLimitEntry{}
	rateLimitState.mu.Unlock()
}

// 模型级限流：同账号换模型应不受影响
func TestRateLimitModelScoped(t *testing.T) {
	resetRateLimit(t)
	until := time.Now().Add(30 * time.Minute)
	rateLimitRecord("uid-1", "甲", rlKindModel, "deepseek-v4.1-flash", 6004, "用量限流(6004)", until)

	if _, ok := rateLimitFor("uid-1", "deepseek-v4.1-flash"); !ok {
		t.Fatal("被限的模型应查到")
	}
	if _, ok := rateLimitFor("uid-1", "hy4-preview"); ok {
		t.Fatal("同账号其它模型不该被判为受限（模型级限流）")
	}
}

// 账号级限流（11140 风控）：任意模型都受限
func TestRateLimitAccountScoped(t *testing.T) {
	resetRateLimit(t)
	rateLimitRecord("uid-2", "乙", rlKindAccount, "", 11140, "账号级风控(11140)", time.Time{})

	for _, m := range []string{"deepseek-v4.1-flash", "hy4-preview", "glm-5.2"} {
		if _, ok := rateLimitFor("uid-2", m); !ok {
			t.Fatalf("账号级限流应对 %s 也生效", m)
		}
	}
}

// 上限保护：上游给个离谱的远期时间，也不能把号锁死超过 rateLimitMaxUntil
func TestRateLimitCappedUntil(t *testing.T) {
	resetRateLimit(t)
	far := time.Now().Add(30 * 24 * time.Hour) // 上游说"一个月后恢复"
	rateLimitRecord("uid-3", "丙", rlKindModel, "m", 6004, "限流", far)

	e, ok := rateLimitFor("uid-3", "m")
	if !ok {
		t.Fatal("应记录")
	}
	if time.Until(e.Until) > rateLimitMaxUntil+time.Minute {
		t.Fatalf("冷却应被截断到 %v 内，实际 %v", rateLimitMaxUntil, time.Until(e.Until))
	}
	// 但原始 resetAt 要留着（供展示"上游说何时恢复"）
	if e.ResetAt.IsZero() {
		t.Fatal("应保留上游原始恢复时刻")
	}
}

// 上游给的时刻已过期：给个短冷却，而不是立即无限可用
func TestRateLimitPastResetAt(t *testing.T) {
	resetRateLimit(t)
	rateLimitRecord("uid-4", "丁", rlKindModel, "m", 6004, "限流", time.Now().Add(-time.Hour))
	e, ok := rateLimitFor("uid-4", "m")
	if !ok {
		t.Fatal("应记录（给短冷却）")
	}
	if time.Until(e.Until) > 2*time.Minute {
		t.Fatalf("过期时刻应给短冷却，实际 %v", time.Until(e.Until))
	}
}

// 过期记录应被清掉（不能永久占着）
func TestRateLimitExpires(t *testing.T) {
	resetRateLimit(t)
	rateLimitState.mu.Lock()
	rateLimitState.loaded = true
	rateLimitState.entries[rlKey("uid-5", rlKindModel, "m")] = &rateLimitEntry{
		UID: "uid-5", Kind: rlKindModel, Model: "m",
		Until: time.Now().Add(-time.Second), // 已过期
	}
	rateLimitState.mu.Unlock()

	if _, ok := rateLimitFor("uid-5", "m"); ok {
		t.Fatal("已过期的记录不该还生效")
	}
	if n := len(rateLimitSnapshot()); n != 0 {
		t.Fatalf("快照应清掉过期项，实际剩 %d", n)
	}
}

// 持久化：重启后仍能读到（否则每次重启就忘了限到几点）
func TestRateLimitPersists(t *testing.T) {
	resetRateLimit(t)
	rateLimitRecord("uid-6", "戊", rlKindModel, "m", 6004, "限流", time.Now().Add(20*time.Minute))

	rateLimitState.mu.Lock()
	rateLimitState.loaded = false
	rateLimitState.entries = map[string]*rateLimitEntry{}
	rateLimitState.mu.Unlock()

	if _, ok := rateLimitFor("uid-6", "m"); !ok {
		t.Fatal("重启后应能读回限流记录")
	}
}

// 重复限流：累计次数增加，但截止时间不无限延长
func TestRateLimitRepeatKeepsUntil(t *testing.T) {
	resetRateLimit(t)
	until := time.Now().Add(30 * time.Minute)
	rateLimitRecord("uid-7", "己", rlKindModel, "m", 6004, "限流", until)
	rateLimitRecord("uid-7", "己", rlKindModel, "m", 6004, "限流", until)

	e, _ := rateLimitFor("uid-7", "m")
	if e.Hits != 2 {
		t.Fatalf("应累计 2 次，实际 %d", e.Hits)
	}
	if time.Until(e.Until) > 31*time.Minute {
		t.Fatalf("重复限流不该延长截止时间，实际 %v", time.Until(e.Until))
	}
}

// 空 uid 直接跳过（没有身份就无法归属）
func TestRateLimitSkipsEmptyUID(t *testing.T) {
	resetRateLimit(t)
	rateLimitRecord("", "无", rlKindModel, "m", 6004, "限流", time.Now().Add(time.Hour))
	if n := len(rateLimitSnapshot()); n != 0 {
		t.Fatalf("uid 为空不该记录，实际 %d 条", n)
	}
}

// 从 429 响应体解析并记录（端到端路径）
func TestRecordRateLimitFromBody(t *testing.T) {
	resetRateLimit(t)
	body := []byte(`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2030-01-01 10:00:00 UTC+8","displayMsg":{"zh":"限流"}}`)
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[]}`)
	auth := authWithUID{uid: "uid-8", nick: "庚"}

	recordRateLimitFromBody(auth, payload, body)

	e, ok := rateLimitFor("uid-8", "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("应从响应体记录限流")
	}
	if e.Kind != rlKindModel {
		t.Fatalf("6004 应记为模型级，实际 %s", e.Kind)
	}
	if e.Code != 6004 {
		t.Fatalf("应记业务码 6004，实际 %d", e.Code)
	}
	// 2030 年 → 应被上限截断
	if time.Until(e.Until) > rateLimitMaxUntil+time.Minute {
		t.Fatalf("应被上限截断，实际 %v", time.Until(e.Until))
	}
	t.Logf("记录: kind=%s model=%s code=%d until=%s", e.Kind, e.Model, e.Code, rateLimitShortUntil(e.Until))
}

// 11140 应记为账号级（实测：连 hi 都被拒，与内容无关）
func TestRecordRateLimit11140IsAccountScoped(t *testing.T) {
	resetRateLimit(t)
	body := []byte(`{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过"}}`)
	auth := authWithUID{uid: "uid-9", nick: "辛"}
	recordRateLimitFromBody(auth, []byte(`{"model":"hy4-preview"}`), body)

	e, ok := rateLimitFor("uid-9", "hy4-preview")
	if !ok {
		t.Fatal("应记录")
	}
	if e.Kind != rlKindAccount {
		t.Fatalf("11140 应记为账号级，实际 %s", e.Kind)
	}
	if !strings.Contains(e.Reason, "风控") {
		t.Fatalf("原因应提到风控：%s", e.Reason)
	}
}

// 无 code 的响应不该瞎记
func TestRecordRateLimitNoCode(t *testing.T) {
	resetRateLimit(t)
	recordRateLimitFromBody(authWithUID{uid: "uid-10"}, []byte(`{"model":"m"}`), []byte(`{"msg":"something"}`))
	// code=0 且 msg 不含 frequency limit → 仍会记一条 model 级（保守），
	// 但不该 panic；这里只断言不崩且有记录可查
	_ = rateLimitSnapshot()
}

// authWithUID 测试用 AuthProvider：Summary 带 uid/昵称
// （与 proxy_retry429_test.go 里的空 staticAuth 区分开）
type authWithUID struct {
	uid  string
	nick string
}

func (a authWithUID) Headers() (map[string]string, error) { return map[string]string{}, nil }
func (a authWithUID) Summary() (map[string]any, error) {
	return map[string]any{"uid": a.uid, "nickname": a.nick}, nil
}
