package main

// 2026-11-02：日志轮转的回归。
//
// 起因：hub.log / converter-*.log 无限追加（实测 converter-qoder.log 已 18MB），
// 出问题翻日志要 tail 半天。这里把「超过阈值就滚动、保留 N 份」的行为钉住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withRotateLimits 临时调小阈值/保留份数，测试结束恢复。
func withRotateLimits(t *testing.T, maxBytes int64, keep int) {
	t.Helper()
	oldMax, oldKeep := logMaxBytesHook, logKeepHook
	logMaxBytesHook = func() int64 { return maxBytes }
	logKeepHook = func() int { return keep }
	t.Cleanup(func() { logMaxBytesHook, logKeepHook = oldMax, oldKeep })
}

func TestFileLoggerRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "converter-x.log")
	withRotateLimits(t, 200, 3)

	l := NewFileLogger(path)
	if l == nil {
		t.Fatal("NewFileLogger 返回 nil")
	}
	// 每行 ~50 字节，写 20 行 ≈ 1000 字节 → 至少要滚 3 次
	for i := 0; i < 20; i++ {
		l.Log("这是第 %02d 行，用来把文件撑过阈值。", i)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("当前日志文件应存在：%v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("应生成 %s.1", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 400 {
		t.Errorf("当前文件应远小于总量，实际 %d 字节（轮转没生效？）", fi.Size())
	}
	// 保留份数上限：keep=3 → 最多 .1 .2 .3，不该出现 .4
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("keep=3 时不该出现 .4")
	}
	// 历史文件里应能看到更早的内容
	b, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "这是第") {
		t.Errorf(".1 里应是上一次写满的内容，实际：%q", string(b[:min(len(b), 80)]))
	}
}

func TestLogHubRotates(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	withRotateLimits(t, 150, 2)

	for i := 0; i < 30; i++ {
		logHub("填充日志行 %02d", i)
	}
	path := filepath.Join(hubRoot, "hub.log")
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("hub.log 应被轮转出 .1：%v", err)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("keep=2 时不该出现 .3")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 300 {
		t.Errorf("hub.log 当前应远小于总量，实际 %d", fi.Size())
	}
}

func TestRotateKeepsNewestContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	withRotateLimits(t, 1<<20, 3) // 阈值很大：这次只手工制造超大文件验证「打开时轮转」
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	logMaxBytesHook = func() int64 { return 100 } // 打开时判定超大
	l := NewFileLogger(path)
	if l == nil {
		t.Fatal("打开失败")
	}
	l.Log("新内容")
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("NewFileLogger 打开超大文件时应先滚一份：%v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "新内容") {
		t.Errorf("新文件应包含刚写的内容，实际 %q", string(b))
	}
	old, _ := os.ReadFile(path + ".1")
	if len(old) != 500 {
		t.Errorf(".1 应保留原来的 500 字节，实际 %d", len(old))
	}
}

func TestRotateEnvDefaults(t *testing.T) {
	// 默认 10MB / 3 份（不设环境变量时）
	t.Setenv("AIHUB_LOG_MAX_MB", "")
	t.Setenv("AIHUB_LOG_KEEP", "")
	logMaxBytesHook, logKeepHook = func() int64 { return int64(logMaxMBDefault) << 20 }, func() int { return logKeepDefault }
	if got := logMaxBytesHook(); got != 10<<20 {
		t.Errorf("默认阈值应为 10MB，实际 %d", got)
	}
	if got := logKeepHook(); got != 3 {
		t.Errorf("默认保留应为 3 份，实际 %d", got)
	}
}
