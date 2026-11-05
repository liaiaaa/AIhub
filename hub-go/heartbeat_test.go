package main

// 客户端心跳模拟（heartbeat.go）的离线测试。
// 全部注入假探针，不发任何上游请求。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func resetActivity() {
	activityState.mu.Lock()
	activityState.last = map[string]time.Time{}
	activityState.mu.Unlock()
}

func testHeartbeatState(t *testing.T) *heartbeatState {
	t.Helper()
	hb := newHeartbeatState()
	hb.cfg = defaultHeartbeatSettings()
	hb.cfg.MinIntervalSec = 60
	hb.cfg.MaxIntervalSec = 60
	hb.cfg.QuietFromHour = 0
	hb.cfg.QuietToHour = 0 // 关掉静默，避免测试跑在凌晨时行为随机器时间变化
	return hb
}

func TestHeartbeatSettingsNormalize(t *testing.T) {
	s := heartbeatSettings{MinIntervalSec: 5, MaxIntervalSec: 3, IdleGateSec: -1, MaxPerDay: -5,
		QuietFromHour: 30, QuietToHour: -2}.normalized()
	if s.MinIntervalSec != 60 {
		t.Fatalf("最小间隔应抬到 60s，得到 %d", s.MinIntervalSec)
	}
	if s.MaxIntervalSec != 60 {
		t.Fatalf("最大间隔应不小于最小间隔，得到 %d", s.MaxIntervalSec)
	}
	if s.IdleGateSec != 0 || s.MaxPerDay != 0 {
		t.Fatalf("负值应归零：%+v", s)
	}
	if s.QuietFromHour != 0 || s.QuietToHour != 0 {
		t.Fatalf("越界小时应归零：%+v", s)
	}
}

func TestHeartbeatEnvOverride(t *testing.T) {
	dir := t.TempDir()
	old := hubRoot
	hubRoot = dir
	defer func() { hubRoot = old }()

	t.Setenv("AIHUB_HEARTBEAT", "0")
	t.Setenv("AIHUB_HEARTBEAT_MIN", "120")
	t.Setenv("AIHUB_HEARTBEAT_MAX", "300")
	t.Setenv("AIHUB_HEARTBEAT_MAXDAY", "7")
	s := loadHeartbeatSettings()
	if s.Enabled {
		t.Fatal("AIHUB_HEARTBEAT=0 应关闭")
	}
	if s.MinIntervalSec != 120 || s.MaxIntervalSec != 300 || s.MaxPerDay != 7 {
		t.Fatalf("环境变量覆盖没生效：%+v", s)
	}
}

func TestHeartbeatQuietWindow(t *testing.T) {
	cfg := defaultHeartbeatSettings() // 01:00~07:00
	at := func(h, m int) time.Time { return time.Date(2026, 11, 5, h, m, 0, 0, time.Local) }
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(0, 30), false},
		{at(1, 0), true},
		{at(3, 15), true},
		{at(6, 59), true},
		{at(7, 0), false},
		{at(14, 0), false},
	}
	for _, c := range cases {
		if got := heartbeatQuiet(c.t, cfg); got != c.want {
			t.Fatalf("%s 期望 quiet=%v，得到 %v", c.t.Format("15:04"), c.want, got)
		}
	}
	// 跨午夜窗口 23~6
	wrap := cfg
	wrap.QuietFromHour, wrap.QuietToHour = 23, 6
	if !heartbeatQuiet(at(23, 30), wrap) || !heartbeatQuiet(at(2, 0), wrap) {
		t.Fatal("跨午夜静默窗口判断错误")
	}
	if heartbeatQuiet(at(12, 0), wrap) {
		t.Fatal("12:00 不应在 23~6 静默窗内")
	}
}

func TestHeartbeatDelayWithinRange(t *testing.T) {
	cfg := heartbeatSettings{MinIntervalSec: 60, MaxIntervalSec: 61}
	for i := 0; i < 50; i++ {
		d := heartbeatDelay(cfg)
		if d != 60*time.Second && d != 61*time.Second {
			t.Fatalf("间隔越界：%s", d)
		}
	}
}

func countingProbe(line string, n *int32, fail bool) heartbeatProbe {
	return heartbeatProbe{Line: line, Label: "test", Run: func(ctx context.Context) (string, error) {
		atomic.AddInt32(n, 1)
		if fail {
			return "", context.DeadlineExceeded
		}
		return "ok", nil
	}}
}

func TestHeartbeatTickRunsProbe(t *testing.T) {
	resetActivity()
	hb := testHeartbeatState(t)
	var n int32
	hb.runHeartbeatTick([]heartbeatProbe{countingProbe("dom", &n, false)})
	if atomic.LoadInt32(&n) != 1 {
		t.Fatalf("应发 1 次，实际 %d", n)
	}
	st := hb.lines["dom"]
	if st == nil || st.Count != 1 || !st.LastOK || hb.dayCount != 1 {
		t.Fatalf("状态记账不对：%+v dayCount=%d", st, hb.dayCount)
	}
}

func TestHeartbeatTickRespectsDisabledAndLimits(t *testing.T) {
	resetActivity()
	var n int32

	// 关闭
	hb := testHeartbeatState(t)
	hb.cfg.Enabled = false
	hb.runHeartbeatTick([]heartbeatProbe{countingProbe("dom", &n, false)})
	if n != 0 {
		t.Fatal("关闭状态下不应发请求")
	}

	// 日上限
	hb = testHeartbeatState(t)
	hb.cfg.MaxPerDay = 1
	hb.dayCount = 1
	hb.runHeartbeatTick([]heartbeatProbe{countingProbe("dom", &n, false)})
	if n != 0 {
		t.Fatal("达到日上限后不应发请求")
	}

	// 没有探针
	hb = testHeartbeatState(t)
	hb.runHeartbeatTick(nil)
	if hb.lastSkip == "" {
		t.Fatal("没有探针时应记录 skip 原因")
	}
}

func TestHeartbeatTickIdleGate(t *testing.T) {
	resetActivity()
	hb := testHeartbeatState(t)
	hb.cfg.IdleGateSec = 90
	var n int32
	noteRealTraffic("dom")

	hb.runHeartbeatTick([]heartbeatProbe{countingProbe("dom", &n, false)})
	if n != 0 {
		t.Fatal("刚有真实请求时应跳过心跳")
	}
	if !strings.Contains(hb.lastSkip, "真实请求") {
		t.Fatalf("skip 原因不对：%q", hb.lastSkip)
	}

	// 别的线路有流量不影响本条
	resetActivity()
	noteRealTraffic("intl")
	hb.runHeartbeatTick([]heartbeatProbe{countingProbe("dom", &n, false)})
	if n != 1 {
		t.Fatal("其他线路的流量不应拦住本条心跳")
	}
}

func TestHeartbeatFailureAccounting(t *testing.T) {
	resetActivity()
	hb := testHeartbeatState(t)
	var n int32
	hb.runHeartbeatTick([]heartbeatProbe{countingProbe("zcode", &n, true)})
	st := hb.lines["zcode"]
	if st == nil || st.Fail != 1 || st.ConsecFail != 1 || st.LastOK {
		t.Fatalf("失败记账不对：%+v", st)
	}
	if hb.dayCount != 0 {
		t.Fatal("失败的探针不应计入成功配额")
	}
}

func TestPickProbePrefersLeastRecentlyUsed(t *testing.T) {
	hb := testHeartbeatState(t)
	hb.lines["dom"] = &heartbeatLineState{Line: "dom", LastAt: time.Now().Add(-time.Minute)}
	hb.lines["intl"] = &heartbeatLineState{Line: "intl", LastAt: time.Now().Add(-time.Hour)}
	probes := []heartbeatProbe{{Line: "dom"}, {Line: "intl"}}
	if got := hb.pickProbe(probes); got.Line != "intl" {
		t.Fatalf("应优先最久没发的线路，得到 %s", got.Line)
	}
	hb.lines["zcode"] = &heartbeatLineState{Line: "zcode"} // 从未发过
	probes = append(probes, heartbeatProbe{Line: "zcode"})
	if got := hb.pickProbe(probes); got.Line != "zcode" {
		t.Fatalf("应优先从未发过的线路，得到 %s", got.Line)
	}
}

func TestHeartbeatSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := hubRoot
	hubRoot = dir
	defer func() { hubRoot = old }()

	cfg := defaultHeartbeatSettings()
	cfg.MinIntervalSec, cfg.MaxIntervalSec = 200, 400
	cfg.IdleGateSec = 30
	if err := saveHeartbeatSettings(cfg); err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	// 落盘权限 0600（里面有节奏参数，不算机密，但别给 group/world 写）
	if st, err := os.Stat(heartbeatSettingsPath()); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("权限不对：%v %v", err, st)
	}
	got := loadHeartbeatSettings()
	if got.MinIntervalSec != 200 || got.MaxIntervalSec != 400 || got.IdleGateSec != 30 {
		t.Fatalf("读回不一致：%+v", got)
	}
}

func TestAPIHeartbeatPatch(t *testing.T) {
	dir := t.TempDir()
	old := hubRoot
	hubRoot = dir
	defer func() { hubRoot = old }()

	h := &Hub{}
	h.hb = newHeartbeatState()
	body := `{"enabled":false,"min_interval_sec":180,"max_interval_sec":360}`
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.apiHeartbeat(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rr.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("接口返回失败：%s", rr.Body.String())
	}
	if h.hb.cfg.Enabled || h.hb.cfg.MinIntervalSec != 180 || h.hb.cfg.MaxIntervalSec != 360 {
		t.Fatalf("内存配置未更新：%+v", h.hb.cfg)
	}
	if !fileExists(heartbeatSettingsPath()) {
		t.Fatal("配置未落盘")
	}

	// GET 返回状态
	req = httptest.NewRequest(http.MethodGet, "/api/heartbeat", nil)
	rr = httptest.NewRecorder()
	h.apiHeartbeat(rr, req)
	var view map[string]any
	json.Unmarshal(rr.Body.Bytes(), &view)
	if view["heartbeat"] == nil {
		t.Fatalf("GET 未返回 heartbeat：%s", rr.Body.String())
	}
}

func TestHeartbeatStatusWithoutStart(t *testing.T) {
	h := &Hub{}
	st := h.heartbeatStatus()
	if st["enabled"] != false || st["state"] != "未启动" {
		t.Fatalf("未启动时的状态不对：%+v", st)
	}
}

func TestHeartbeatUnsupportedLinesDocumented(t *testing.T) {
	unsupported := heartbeatUnsupportedLines()
	for _, key := range []string{"trae", "oc", "mimo"} {
		if unsupported[key] == "" {
			t.Fatalf("%s 没写不发的理由", key)
		}
	}
}

func TestHeartbeatWBCatalogPullMissingCred(t *testing.T) {
	p := &Proxy{Name: "intl", Backend: "https://example.invalid", DefaultDomain: "example.invalid", UserAgent: "test"}
	p.AuthDir = t.TempDir() // 空目录：没有凭证文件
	if _, err := heartbeatWBCatalogPull("intl", p); err == nil {
		t.Fatal("没有凭据时应报错，而不是发请求")
	}
}

func TestHeartbeatLoopWakesAndTicks(t *testing.T) {
	resetActivity()
	h := &Hub{}
	hb := newHeartbeatState()
	hb.cfg = heartbeatSettings{Enabled: true, MinIntervalSec: 1, MaxIntervalSec: 1,
		IdleGateSec: 0, MaxPerDay: 100, QuietFromHour: 0, QuietToHour: 0}
	h.hb = hb
	go h.heartbeatLoop(hb)
	defer h.StopHeartbeat()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		hb.mu.Lock()
		skip := hb.lastSkip
		hb.mu.Unlock()
		if strings.Contains(skip, "没有在跑的线路") {
			return // 循环确实醒了并跑到了调度逻辑
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("心跳循环 3 秒内没有调度过一次")
}

func TestStopHeartbeatWakesLoop(t *testing.T) {
	h := &Hub{}
	hb := newHeartbeatState()
	hb.cfg = heartbeatSettings{Enabled: true, MinIntervalSec: 3600, MaxIntervalSec: 3600}
	h.hb = hb
	done := make(chan struct{})
	go func() {
		h.heartbeatLoop(hb)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	h.StopHeartbeat()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopHeartbeat 没有立刻唤醒循环（要等 1 小时的定时器）")
	}
}

func TestHeartbeatEmptyEnvKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	old := hubRoot
	hubRoot = dir
	defer func() { hubRoot = old }()
	t.Setenv("AIHUB_HEARTBEAT", "") // 启动脚本里留空的常见写法
	if s := loadHeartbeatSettings(); !s.Enabled {
		t.Fatal("AIHUB_HEARTBEAT 为空应按「没设」处理，不该关闭心跳")
	}
}
