package main

// 2026-10-19：qoder dump 无清理机制，数据根积了 12 份 26MB（每份 0.5-1.2MB，
// 整个请求信封）。新增 qoderPruneDumps：落盘后按修改时间保留最近 5 份。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQoderPruneDumpsKeepsNewest(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })

	// 造 8 份 dump，修改时间从旧到新（间隔 1 分钟保证 mtime 有区分度）
	base := time.Now().Add(-10 * time.Minute)
	for i := 0; i < 8; i++ {
		p := dataPath("qoder-dump-" + string(rune('a'+i)) + "-upstream.json")
		content := []byte(strings.Repeat("x", 100+i))
		if err := os.WriteFile(p, content, 0644); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	qoderPruneDumps(nil)

	remaining, err := filepath.Glob(dataPath("qoder-dump-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != qoderDumpKeep {
		names := make([]string, 0, len(remaining))
		for _, r := range remaining {
			names = append(names, filepath.Base(r))
		}
		t.Fatalf("应保留 %d 份，实际 %d 份：%v", qoderDumpKeep, len(remaining), names)
	}
	// 留下的必须是**最新的**那 5 份（d,e,f,g,h——a/b/c 最老应被删）
	for _, want := range []string{"d", "e", "f", "g", "h"} {
		found := false
		for _, r := range remaining {
			if strings.Contains(filepath.Base(r), "-"+want+"-") {
				found = true
			}
		}
		if !found {
			t.Errorf("最新的一份 %s 应被保留", want)
		}
	}
	for _, gone := range []string{"a", "b", "c"} {
		for _, r := range remaining {
			if strings.Contains(filepath.Base(r), "-"+gone+"-") {
				t.Errorf("最老的一份 %s 应被删除", gone)
			}
		}
	}
}

// 不够保留数时一个都不删（避免误删）。
func TestQoderPruneDumpsNoopWhenUnderLimit(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })

	for i := 0; i < 3; i++ {
		p := dataPath("qoder-dump-" + string(rune('a'+i)) + "-upstream.json")
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	qoderPruneDumps(nil)
	remaining, _ := filepath.Glob(dataPath("qoder-dump-*.json"))
	if len(remaining) != 3 {
		t.Fatalf("3 份 < 保留数，应一个不删，实际剩 %d", len(remaining))
	}
}

// 只动 qoder-dump-*.json，其它文件不能碰。
func TestQoderPruneDumpsOnlyTouchesDumps(t *testing.T) {
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })

	keep := dataPath("state.json")
	if err := os.WriteFile(keep, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		p := dataPath("qoder-dump-" + string(rune('a'+i)) + "-upstream.json")
		os.WriteFile(p, []byte("x"), 0644)
	}
	qoderPruneDumps(nil)
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("state.json 被误删了")
	}
}
