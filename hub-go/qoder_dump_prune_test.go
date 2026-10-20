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

// 2026-10-20 回归钉：provider_error 与载荷体积**彻底无关**，所以提示不再
// 按体积设门槛——小载荷失败同样要给用户解释。
//
// 决定性对照（同账号、同时段、同模型 dfmodel）：
//   msgs=2    payload=3KB    → 成功（session title）
//   msgs=14   payload=61KB   → provider_error
//   msgs=117  payload=235KB  → provider_error
//   msgs=1517 payload=5.8MB  → provider_error
// 61KB 就失败、3KB 就成功，说明旧门槛（512KB 才提示）是错的。
func TestQoderUpstreamHintAlwaysExplains(t *testing.T) {
	// 关键：小载荷也必须给提示（61KB 实测会失败）
	for _, size := range []int{61 << 10, 235 << 10, 1634 << 10, 5 << 20} {
		got := qoderUpstreamHint(size)
		if got == "" {
			t.Fatalf("载荷 %dKB 失败时不该静默——必须给提示（实测 61KB 就会 provider_error）", size>>10)
		}
		for _, want := range []string{"临时故障", "无关", "Qwen3.8-Flash", "免费"} {
			if !strings.Contains(got, want) {
				t.Errorf("载荷 %dKB 的提示缺 %q：%s", size>>10, want, got)
			}
		}
	}
	// 旧归因必须消失
	got := qoderUpstreamHint(61 << 10)
	for _, bad := range []string{"拒绝该载荷", "上游模型后端拒绝"} {
		if strings.Contains(got, bad) {
			t.Errorf("旧文案把体积当原因：%s", got)
		}
	}
	// payloadLen=0 也要能给出提示（只是不带体积信息）
	if g := qoderUpstreamHint(0); g == "" || strings.Contains(g, "0.0MB") {
		t.Errorf("payloadLen=0 时不该出现 0.0MB，且仍要有提示：%q", g)
	}
}
