package main

// 观测类只读接口的测试（2026-10-31）。
//
// 重点验证三件事：
//  1. 日志文件名必须走白名单，任何路径穿越/未知名都要被拒；
//  2. tailLines 在「文件比 256KB 大」时不能把半行当成一行返回；
//  3. /api/limits 只暴露看板需要的字段（不泄漏上游 UID）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestObsLogFilesWhitelist 白名单里只有 hub + 各线路日志。
func TestObsLogFilesWhitelist(t *testing.T) {
	files := obsLogFiles()
	if _, ok := files["hub"]; !ok {
		t.Error("白名单缺 hub 主日志")
	}
	for _, s := range lineSpecs {
		if _, ok := files[s.Key]; !ok {
			t.Errorf("白名单缺线路 %s 的日志", s.Key)
		}
		if files[s.Key] != s.LogFile {
			t.Errorf("线路 %s 的日志名 = %q，期望 %q", s.Key, files[s.Key], s.LogFile)
		}
	}
}

// TestApiLogsTailRejectsUnknownFile 未知文件名必须 400，不能当路径用。
func TestApiLogsTailRejectsUnknownFile(t *testing.T) {
	h := &Hub{}
	for _, bad := range []string{"../../etc/passwd", "/etc/passwd", "state.json", "converter-other.log"} {
		req := httptest.NewRequest(http.MethodGet, "/api/logs/tail?file="+bad, nil)
		rec := httptest.NewRecorder()
		h.apiLogsTail(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("file=%q 返回 %d，期望 400", bad, rec.Code)
		}
	}
}

// TestApiLogsTailKnownFile 已知文件返回尾部内容，且 lines 上限生效。
func TestApiLogsTailKnownFile(t *testing.T) {
	orig := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = orig })

	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString("line-" + string(rune('a'+i)) + "\n")
	}
	if err := os.WriteFile(filepath.Join(hubRoot, "hub.log"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	h := &Hub{}
	req := httptest.NewRequest(http.MethodGet, "/api/logs/tail?file=hub&lines=3", nil)
	rec := httptest.NewRecorder()
	h.apiLogsTail(rec, req)

	var resp struct {
		OK    bool     `json:"ok"`
		Lines int      `json:"lines"`
		Tail  []string `json:"tail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if !resp.OK || resp.Lines != 3 || len(resp.Tail) != 3 {
		t.Fatalf("期望 3 行，实际 ok=%v lines=%d tail=%v", resp.OK, resp.Lines, resp.Tail)
	}
	if resp.Tail[2] != "line-t" {
		t.Errorf("最后一行 = %q，期望 line-t", resp.Tail[2])
	}
}

// TestTailLinesDropsPartialFirstLine 文件超过尾部窗口时，首行可能是半行，必须丢掉。
func TestTailLinesDropsPartialFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 造一个比 logTailMaxBytes 大的文件，每行 100 字节
	line := strings.Repeat("x", 99) + "\n"
	for written := 0; written < logTailMaxBytes+5000; written += len(line) {
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	tail, err := tailLines(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 5 {
		t.Fatalf("期望 5 行，实际 %d 行", len(tail))
	}
	for i, l := range tail {
		if len(l) != 99 {
			t.Errorf("第 %d 行长度 = %d，期望 99（半行没被丢掉）", i, len(l))
		}
	}
}

// TestApiLimitsDoesNotLeakUID 限流快照不带 uid 字段。
func TestApiLimitsDoesNotLeakUID(t *testing.T) {
	// 放一条即将过期的记录进台账
	uid := "uid-should-not-leak"
	rateLimitState.mu.Lock()
	loadRateLimitLocked()
	rateLimitState.entries[uid] = &rateLimitEntry{
		UID: uid, Name: "测试账号", Kind: "model", Model: "glm-4.6",
		Code: 6004, Reason: "模型限流", Until: time.Now().Add(10 * time.Minute),
		SeenAt: time.Now(), Hits: 2,
	}
	rateLimitState.mu.Unlock()
	t.Cleanup(func() {
		rateLimitState.mu.Lock()
		delete(rateLimitState.entries, uid)
		rateLimitState.mu.Unlock()
	})

	h := &Hub{}
	rec := httptest.NewRecorder()
	h.apiLimits(rec, httptest.NewRequest(http.MethodGet, "/api/limits", nil))

	var resp struct {
		OK     bool             `json:"ok"`
		Limits []map[string]any `json:"limits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if !resp.OK {
		t.Fatal("ok 应为 true")
	}
	for _, item := range resp.Limits {
		if _, leaked := item["uid"]; leaked {
			t.Error("响应里出现了 uid 字段（上游账号标识不该暴露给前端）")
		}
	}
}

// TestHumanBytes 体积文案。
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:              "512 B",
		1024:             "1.0 KB",
		1024 * 1024:      "1.0 MB",
		28 * 1024 * 1024: "28.0 MB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q，期望 %q", in, got, want)
		}
	}
}
