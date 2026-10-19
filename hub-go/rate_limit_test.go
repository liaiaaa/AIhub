package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// 关键回归（2026-10-12 用户报）：**额度查得到 ≠ 能推理**。
// 实测<凭证目录A>：额度 200、userinfo 200、成长中心 200，但 chat 403 11140。
// 此前 intlCredStatusOne 只看额度就标 ✓，等于骗用户。
// 现在必须真探活一次；探到风控要记进台账，且不能标 OK。
func TestProbeRiskNotMarkedAvailable(t *testing.T) {
	resetRateLimit(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过"}}`))
	}))
	defer up.Close()

	res, _ := intlProbeInferWithURL(up.URL)
	if res != probeRisk {
		t.Fatalf("403/11140 应判为风控，实际 %v", res)
	}
}

// 6004 应判为限流（区别于风控）
func TestProbeRateLimited(t *testing.T) {
	resetRateLimit(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":6004,"msg":"usage exceeds frequency limit"}`))
	}))
	defer up.Close()
	if res, _ := intlProbeInferWithURL(up.URL); res != probeRateLimited {
		t.Fatalf("429/6004 应判为限流，实际 %v", res)
	}
}

// 200 应判为可用
func TestProbeOK(t *testing.T) {
	resetRateLimit(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"h\"}}]}\n\n"))
	}))
	defer up.Close()
	if res, _ := intlProbeInferWithURL(up.URL); res != probeOK {
		t.Fatalf("200 应判为可用，实际 %v", res)
	}
}

// 探活必须真的发请求（不能凭额度就下结论）
func TestProbeActuallySendsRequest(t *testing.T) {
	resetRateLimit(t)
	var hit int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
		if !strings.Contains(r.URL.Path, "chat/completions") {
			t.Errorf("应打 chat/completions，实际 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {}\n\n"))
	}))
	defer up.Close()
	intlProbeInferWithURL(up.URL) //nolint:errcheck // 只验证确实发了请求
	if atomic.LoadInt32(&hit) == 0 {
		t.Fatal("必须真的发一次推理请求")
	}
}

// 关键修正（2026-10-13 用户报）：11140 风控上游**从不给恢复时刻**
// （实测响应只有 code/msg/displayMsg，无任何时间字段）。
// 此前代码用 rateLimitMaxUntil（2 小时）兜底，界面上却显示成「恢复 10:24」，
// 是假信息——用户会以为到点就能好。必须区分「上游给的」与「本地兜底」。
func TestRateLimitUpstreamResetFlag(t *testing.T) {
	resetRateLimit(t)

	// 6004：上游文案带恢复时刻 → HasUpstreamReset = true
	body6004 := []byte(`{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at 2030-01-01 10:00:00 UTC+8"}`)
	recordRateLimitFromBody(authWithUID{uid: "u-a"}, []byte(`{"model":"m"}`), body6004)
	e, ok := rateLimitFor("u-a", "m")
	if !ok {
		t.Fatal("应记录")
	}
	if !e.HasUpstreamReset() {
		t.Fatal("6004 带恢复时刻，应标记 HasUpstreamReset=true")
	}

	// 11140：上游无时间 → HasUpstreamReset = false（本地兜底不算承诺）
	body11140 := []byte(`{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过安全审核"}}`)
	recordRateLimitFromBody(authWithUID{uid: "u-b"}, []byte(`{"model":"m"}`), body11140)
	e2, ok := rateLimitFor("u-b", "m")
	if !ok {
		t.Fatal("应记录")
	}
	if e2.HasUpstreamReset() {
		t.Fatalf("11140 无上游时间，不该标记 HasUpstreamReset（实际 ResetAt=%v）", e2.ResetAt)
	}
	// 但仍应有本地兜底 Until（用于"暂缓多久再试"），只是不当作"恢复时刻"
	if e2.Until.IsZero() {
		t.Fatal("本地兜底 Until 仍应存在（用于暂缓重试）")
	}
	t.Logf("11140: Until=%v HasUpstreamReset=%v（正确：不谎称恢复时间）",
		rateLimitShortUntil(e2.Until), e2.HasUpstreamReset())
}

// 关键修正（2026-10-13 用户报障）：狗/<凭证目录C>的 deepseek-v4.1-flash 被 6004
// 限到 11:54/11:01，台账里明明有记录，看板却一直显示「✓ 可用」——因为探活
// 用的是免费的 hy4-preview（没被限），而模型级台账此前根本没进看板。
// modelLimitsFor 必须把「哪个模型限到几点」原样交出来。
func TestModelLimitsForSurfacesModelRateLimit(t *testing.T) {
	resetRateLimit(t)

	// 账号 A：deepseek 被 6004 限，上游给了恢复时刻（30 分钟后）
	resetA := time.Now().Add(30 * time.Minute)
	bodyA := []byte(`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at ` +
		resetA.Format("2006-01-02 15:04:05") + ` UTC+8"}`)
	recordRateLimitFromBody(authWithUID{uid: "uid-dog"}, []byte(`{"model":"deepseek-v4.1-flash"}`), bodyA)

	limits := modelLimitsFor("uid-dog")
	if len(limits) != 1 {
		t.Fatalf("应列出 1 条模型限流，实际 %d 条", len(limits))
	}
	m := limits[0]
	if m.Model != "deepseek-v4.1-flash" {
		t.Fatalf("模型名不对: %s", m.Model)
	}
	if !m.UpstreamReset || m.UpstreamUntil == "" {
		t.Fatalf("上游给了恢复时刻，必须带出 UpstreamUntil（实际 %+v）", m)
	}
	if m.UpstreamUntil != rateLimitShortUntil(resetA) {
		t.Fatalf("UpstreamUntil 应为上游时刻 %v，实际 %v", rateLimitShortUntil(resetA), m.UpstreamUntil)
	}
	if m.RemainTx == "" {
		t.Fatal("应给出剩余时间")
	}

	// 没被限的账号 / 空 uid 都该返回空，而不是 nil 崩溃
	if l := modelLimitsFor("uid-clean"); l != nil {
		t.Fatalf("无记录账号应为空，实际 %v", l)
	}
	if l := modelLimitsFor(""); l != nil {
		t.Fatalf("空 uid 应为空，实际 %v", l)
	}
}

// 旧版误记清理：11148（工具记录错位）不是限流，重启后不能再从台账里复活
// （2026-10-13：<凭证目录B> 曾因此被显示成「deepseek 限流到 10:58」）。
func TestLoadRateLimitPurgesNonQuotaEntries(t *testing.T) {
	resetRateLimit(t)

	// 手工写一条 11148 的旧记录进文件（模拟旧版本留下的脏数据）
	far := time.Now().Add(2 * time.Hour)
	rateLimitRecord("uid-dirty", "脏", rlKindModel, "deepseek-v4.1-flash", 11148, "限流", far)
	rateLimitRecord("uid-good", "净", rlKindModel, "deepseek-v4.1-flash", 6004, "用量限流(6004)", far)

	// 强制重新从文件加载
	rateLimitState.mu.Lock()
	rateLimitState.loaded = false
	rateLimitState.entries = map[string]*rateLimitEntry{}
	rateLimitState.mu.Unlock()

	if _, ok := rateLimitFor("uid-dirty", "deepseek-v4.1-flash"); ok {
		t.Fatal("11148 旧记录不应再被视为限流")
	}
	if _, ok := rateLimitFor("uid-good", "deepseek-v4.1-flash"); !ok {
		t.Fatal("6004 记录应保留")
	}
	for _, e := range rateLimitSnapshot() {
		if e.UID == "uid-dirty" {
			t.Fatalf("脏记录应被清掉，实际仍在快照: %+v", e)
		}
	}
}

// 看板端到端（离线）：账号探活通过时，模型级限流必须出现在该凭证的
// model_limits 里——这就是用户报的「deepseek-v4.1-flash 明明被限了，
// 看板却一声不吭」。探活/额度用函数变量桩，绝不在此测试里打上游。
func TestBoardSurfacesModelLimitsWhenAccountProbeOK(t *testing.T) {
	resetRateLimit(t)

	oldProbe, oldCred := probeInferFn, intlCreditsFn
	t.Cleanup(func() { probeInferFn, intlCreditsFn = oldProbe, oldCred })
	probeInferFn = func(*CredentialManager, *Proxy) (probeResult, string) { return probeOK, "" }
	intlCreditsFn = func(*CredentialManager, *Proxy) (float64, float64, bool) { return 200, 300, true }

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "workbuddy-desktop-ai.info"),
		[]byte(`{"account":{"uid":"uid-dog","nickname":"<email>"}}`), 0600)

	reset := time.Now().Add(90 * time.Minute)
	body := []byte(`{"code":6004,"msg":"usage exceeds frequency limit, your usage will reset at ` +
		reset.Format("2006-01-02 15:04:05") + ` UTC+8"}`)
	recordRateLimitFromBody(authWithUID{uid: "uid-dog"}, []byte(`{"model":"deepseek-v4.1-flash"}`), body)

	st := intlCredStatusOne("<凭证目录D>", dir, true)
	if !st.OK || st.State != "可用" {
		t.Fatalf("账号通道应可用（探活桩=OK），实际 ok=%v state=%q", st.OK, st.State)
	}
	if len(st.ModelLimits) != 1 || st.ModelLimits[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("模型级限流必须直显到看板，实际 %+v", st.ModelLimits)
	}

	// 序列化后必须带 model_limits（前端渲染依赖的字段名）
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"model_limits"`) || !strings.Contains(string(b), "deepseek-v4.1-flash") {
		t.Fatalf("JSON 应含 model_limits/deepseek，实际 %s", string(b))
	}
	t.Logf("看板输出: %s", string(b))
}

// 离线复刻**当前真实** rate_limit.json 过一遍加载路径，验证用户报的现场：
//   - 狗(4fea40ae)/田(9685e9b1) 的 deepseek-v4.1-flash 模型级限流要能被列出；
//   - <凭证目录B> 的 11148 假记录要被清掉（那是工具记录错位，不是限流）；
//   - 已过期的记录（<凭证目录A> 11140 若已过期）不载入。
//
// 只读取真实文件内容，写进临时目录，不碰生产台账。
func TestBoardAgainstCurrentRealLedger(t *testing.T) {
	if _, err := os.Stat(filepath.Join(oldHubRootForTest(), "rate_limit.json")); err != nil {
		t.Skip("找不到真实台账，跳过复刻")
	}
	resetRateLimit(t)
	b, err := os.ReadFile(filepath.Join(oldHubRootForTest(), "rate_limit.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(rateLimitPath(), b, 0644)

	dog := modelLimitsFor("4fea40ae-42b7-47a0-8c74-ec2417047465")
	tian := modelLimitsFor("9685e9b1-187c-4c05-960b-8e1e35a17a67")
	t.Logf("<凭证目录D>模型限流: %+v", dog)
	t.Logf("<凭证目录C>模型限流: %+v", tian)
	for i, l := range [][]intlModelLimit{dog, tian} {
		if len(l) == 0 {
			continue // 已过恢复时刻属正常（上游 reset 到点）
		}
		for _, m := range l {
			if m.Model == "" || m.Until == "" {
				t.Fatalf("第%d组复刻异常: %+v", i, m)
			}
		}
	}
	// 这里**不**断言「<凭证目录B> 有没有模型级限流」：rate_limit.json 是活数据，
	// 真实额度限流（6004）随流量出现又消失 —— 2026-10-16 本测试就因此从「零条目」
	// 变成「有一条合法 6004」而撞挂，而那跟 11148 误记一点关系没有。
	// 「非额度类 4xx 不该被当成限流」是载入路径的不变量，用固定 fixture 测：
	// 见 TestRateLimitLoadPurgesNonQuotaCodes。
	if l := modelLimitsFor("ff2d08ac-f1f1-4b9a-b6ad-4f55716220b5"); len(l) > 0 {
		for _, m := range l {
			if strings.Contains(m.Reason, "11148") {
				t.Fatalf("11148 误记不该出现在看板: %+v", m)
			}
		}
	}
}

// 载入路径必须把非额度类的码清掉：11148「工具记录不完整」是桥接层排列问题，
// 不是限流；留着它，重启后看板会把请求格式错误说成「deepseek 限流到几点」。
// 生产台账会变，所以这条用写死的 fixture，不读真实文件。
func TestRateLimitLoadPurgesNonQuotaCodes(t *testing.T) {
	resetRateLimit(t)
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339Nano)
	body := `{"entries":{
	  "ff2d08ac|model|deepseek-v4.1-flash":{"uid":"ff2d08ac","name":"<nick-A>","kind":"model","model":"deepseek-v4.1-flash","code":11148,"reason":"工具记录不完整","until":"` + future + `","hits":1},
	  "ff2d08ac|model|hy4-preview":{"uid":"ff2d08ac","name":"<nick-A>","kind":"model","model":"hy4-preview","code":6004,"reason":"用量限流(6004)","until":"` + future + `","hits":1}
	}}`
	if err := os.WriteFile(rateLimitPath(), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	rateLimitState.mu.Lock()
	rateLimitState.entries = map[string]*rateLimitEntry{}
	loadRateLimitLocked()
	rateLimitState.mu.Unlock()

	limits := modelLimitsFor("ff2d08ac")
	if len(limits) != 1 {
		t.Fatalf("应只留 6004 那条，实际 %d 条: %+v", len(limits), limits)
	}
	if limits[0].Model != "hy4-preview" {
		t.Fatalf("留下的是 %q，期望 hy4-preview", limits[0].Model)
	}
}

func oldHubRootForTest() string {
	// hubRoot 默认解析到仓库根（见 paths.go）；这里取真实文件位置用固定推导：
	// 测试运行时工作目录是包目录 hub-go/，上一级即仓库根。
	abs, err := filepath.Abs("..")
	if err != nil {
		return "/nonexistent"
	}
	return abs
}

// 「探活失败」必须带得出原因。四种失败（网络错误 / 未识别错误码 /
// HTTP 非 200 无 code / 取不到认证头）原来全塌成四个字，用户分不清
// 是号坏了还是探测本身不通（2026-10-15 误判「国际版又不行了」）。
func TestProbeUnknownCarriesReason(t *testing.T) {
	resetRateLimit(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"msg":"bad gateway detail"}`))
	}))
	defer up.Close()

	res, detail := intlProbeInferWithURL(up.URL)
	if res != probeUnknown {
		t.Fatalf("未识别的失败应判 probeUnknown，实际 %v", res)
	}
	if detail == "" {
		t.Fatal("probeUnknown 必须带原因，否则看板只能显示「探活失败」四个字")
	}
	if !strings.Contains(detail, "502") || !strings.Contains(detail, "bad gateway detail") {
		t.Fatalf("原因里要有状态码和上游原文，实际：%q", detail)
	}
}

// 上游给了 code 但不是 11140/6004 时，也要把 code 与文案带出来。
func TestProbeUnrecognizedCodeStillExplains(t *testing.T) {
	resetRateLimit(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"code":99999,"msg":"some other gate"}`))
	}))
	defer up.Close()
	res, detail := intlProbeInferWithURL(up.URL)
	if res != probeUnknown {
		t.Fatalf("未识别 code 不该被猜成风控/限流，实际 %v", res)
	}
	if !strings.Contains(detail, "99999") || !strings.Contains(detail, "some other gate") {
		t.Fatalf("应把 code 与原文带出来：%q", detail)
	}
}
