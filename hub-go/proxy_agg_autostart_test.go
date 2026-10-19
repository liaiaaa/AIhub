package main

import (
	"time"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-10-12 事故：聚合口 :8790 在 hub 重启后不自动监听，Codex 桌面版
// 正好在这个空窗里拉模型目录 → 拉不到 → 回退到 codex 二进制内置的官方
// 目录（gpt-5.6-sol 等），且目录只在启动时读一次、不会自愈。
//
// 修法：白名单非空即自启；但用户手动停过就不再自启（尊重意图）。
// 只豁免 :8790 这一个口，其余线路仍保持「启动后默认关闭」。

// useTestAggPort 把聚合口换到一个不会撞上正式实例的端口（正式实例常驻 :8790）。
func useTestAggPort(t *testing.T) {
	t.Helper()
	old := aggPort
	aggPort = 18790
	t.Cleanup(func() { aggPort = old })
}

// isolateAggRoot 把数据根换到临时目录，避免污染真实 agg_models.json / usage.jsonl。
//
// 2026-10-19：race detector 抓到数据竞争 —— 清理阶段还原 hubRoot 时，
// startAggLine 起的后台 goroutine 还在 logHub → dataPath 读 hubRoot。
// 生产代码没有这个问题（hubRoot 进程启动后不再改），是测试自身的时序问题，
// 但它会让 `go test -race` 永远报红，掩盖真实的并发缺陷。
// 修法：cleanup 里先停掉聚合口（停 goroutine），再还原 hubRoot。
func isolateAggRoot(t *testing.T) {
	t.Helper()
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() {
		// 先让后台 goroutine 停干净，再动 hubRoot
		h := &Hub{}
		h.stopAggLine()
		// 给已发出的 goroutine 一点收尾时间（它们只做日志，很快）
		time.Sleep(50 * time.Millisecond)
		hubRoot = old
	})
}

// 自启后 :8790 必须真的在监听（验收标准第 1 条）
func TestAggAutostartListens(t *testing.T) {
	isolateAggRoot(t)
	useTestAggPort(t)
	aggMu.Lock()
	aggList = []aggModel{{Line: "intl", Slug: "intl.x", Backend: "x", Display: "X"}}
	aggMu.Unlock()
	aggSrvMu.Lock()
	aggSrv, aggUserStopped = nil, false
	aggSrvMu.Unlock()
	h := &Hub{}
	h.aggAutostart()

	if !portListening(aggPort) {
		t.Fatalf("自启后 :%d 未在监听", aggPort)
	}
	// 目录接口应能拉到模型（验收标准第 2 条的核心）
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models?client_version=0.162.0", aggPort))
	if err != nil {
		t.Fatalf("拉目录失败：%v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("目录不是合法 JSON：%s", b)
	}
	// 目录 = 白名单。2026-10-15 起 gpt-* 官方模型不再进目录（用户要求目录只放
	// hub-go 线路模型，不混官方模型）；透传路由保留，直接请求 gpt-* 仍走官方订阅。
	want := 1
	if n := len(asSlice(m["models"])); n != want {
		t.Fatalf("目录应有 %d 个模型（白名单 1），实际 %d：%s", want, n, b)
	}
}

// 白名单为空时不该监听（没配模型就没必要占端口）
func TestAggAutostartSkipsWhenEmpty(t *testing.T) {
	isolateAggRoot(t)
	useTestAggPort(t)
	aggMu.Lock()
	aggList = nil
	aggMu.Unlock()
	aggSrvMu.Lock()
	aggSrv, aggUserStopped = nil, false
	aggSrvMu.Unlock()
	t.Cleanup(func() {
		h := &Hub{}
		h.stopAggLine()
	})

	h := &Hub{}
	h.aggAutostart()
	if portListening(aggPort) {
		t.Fatalf("白名单为空却监听了 :%d", aggPort)
	}
}

// 用户手动停过 → 本次运行不再自动拉起（尊重用户意图）
func TestAggAutostartRespectsManualStop(t *testing.T) {
	isolateAggRoot(t)
	useTestAggPort(t)
	aggMu.Lock()
	aggList = []aggModel{{Line: "intl", Slug: "intl.x", Backend: "x", Display: "X"}}
	aggMu.Unlock()
	aggSrvMu.Lock()
	aggSrv, aggUserStopped = nil, true // 模拟用户点过「停止聚合」
	aggSrvMu.Unlock()
	t.Cleanup(func() {
		aggSrvMu.Lock()
		aggUserStopped = false
		aggSrvMu.Unlock()
		h := &Hub{}
		h.stopAggLine()
	})

	h := &Hub{}
	h.aggAutostart()
	if portListening(aggPort) {
		t.Fatalf("用户已手动停止，不该自动拉起")
	}
}

// /api/agg/stop 必须打上「用户已停止」标记
func TestApiAggStopMarksUserStopped(t *testing.T) {
	isolateAggRoot(t)
	useTestAggPort(t)
	aggSrvMu.Lock()
	aggSrv, aggUserStopped = nil, false
	aggSrvMu.Unlock()
	t.Cleanup(func() {
		aggSrvMu.Lock()
		aggUserStopped = false
		aggSrvMu.Unlock()
	})

	h := &Hub{}
	req := httptest.NewRequest("POST", "/api/agg/stop", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.apiAggStop(rec, req)

	aggSrvMu.Lock()
	got := aggUserStopped
	aggSrvMu.Unlock()
	if !got {
		t.Fatalf("调用 /api/agg/stop 后 aggUserStopped 应为 true")
	}
}

// 「未知模型」的 400 也要落一行统计（否则客户端发来的怪模型名在用量表里
// 看不见）。注意 2026-10-13 起 gpt-* 是官方透传、不再走 400，所以这里用
// 非官方的未知名验证同一条记账路径。
func TestUnknownModelRecordedInUsage(t *testing.T) {
	isolateAggRoot(t)
	useTestAggPort(t)
	usageMu.Lock()
	usageCacheRows, usageCacheOff, usageCacheSize = nil, 0, 0
	usageMu.Unlock()

	h := &Hub{}
	body := `{"model":"totally-unknown-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.aggDispatch(rec, req, "responses")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知模型应 400，实际 %d", rec.Code)
	}
	b, err := os.ReadFile(filepath.Join(hubRoot, usageFile))
	if err != nil {
		t.Fatalf("未落统计：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	last := lines[len(lines)-1]
	var rec2 usageRec
	if err := json.Unmarshal([]byte(last), &rec2); err != nil {
		t.Fatalf("统计行不是合法 JSON：%s", last)
	}
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("统计 code 应为 400，实际 %d", rec2.Code)
	}
	if rec2.Slug != "totally-unknown-model" {
		t.Fatalf("统计应记客户端请求的原始名字，实际 %q", rec2.Slug)
	}
	if rec2.Line != "agg" {
		t.Fatalf("统计 line 应为 agg，实际 %q", rec2.Line)
	}
	// 用量表要能显示它的中文名（不能是空字符串）
	if aggLineCN["agg"] == "" {
		t.Fatalf("aggLineCN 缺 agg 键，用量表会显示空名字")
	}
}

// 「agg」不能当成可挂模型的线路（aggLineCN 含它，故必须用独立校验）
func TestAggLineNotSelectable(t *testing.T) {
	if aggSelectableLine("agg") {
		t.Fatalf("agg 是统计归属，不是可挂模型的线路")
	}
	for _, ok := range []string{"dom", "intl", "qoder", "trae"} {
		if !aggSelectableLine(ok) {
			t.Fatalf("%s 应是可挂模型的线路", ok)
		}
	}
	if aggSelectableLine("nope") {
		t.Fatalf("未知线路不该通过校验")
	}
}
