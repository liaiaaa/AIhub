package main

// 2026-10-19 线路自动恢复的回归测试。
//
// 背景：2026-10-19 13:31 重启后只起了 agg+hub，dom/intl/qoder 全丢，
// qoder 一直挂到被发现（聚合网关里两个 qoder 模型持续 503）。
// 根因是「线路在跑」这个事实只存在于内存，进程一退就丢。
// 现在落盘到 state.json 的 last_lines，启动时由 restoreLines 恢复。

import (
	"os"
	"path/filepath"
	"testing"
)

// 记住 → 摘掉 → 顺序稳定，是这套机制的地基。
func TestSanitizeLineNames(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"dom"}, []string{"dom"}},
		// 未知线路名必须被丢弃：否则会去调一个不存在的 /api/<name>/start
		{[]string{"dom", "不存在的线路", "intl"}, []string{"dom", "intl"}},
		// 去重
		{[]string{"dom", "dom", "intl"}, []string{"dom", "intl"}},
		// 输出顺序固定为 knownLineNames 的顺序，与传入顺序无关
		{[]string{"oc", "dom", "qoder"}, []string{"dom", "qoder", "oc"}},
		// 空串/空白忽略
		{[]string{"", "  ", "intl"}, []string{"intl"}},
	}
	for _, c := range cases {
		got := sanitizeLineNames(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("sanitizeLineNames(%v) = %v，期望 %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("sanitizeLineNames(%v) = %v，期望 %v", c.in, got, c.want)
			}
		}
	}
}

// rememberLine 要幂等：重复记同一条线路不该产生重复项，也不该反复写盘。
func TestRememberLineIdempotent(t *testing.T) {
	h := &Hub{}
	h.rememberLine("dom")
	h.rememberLine("dom")
	h.rememberLine("dom")
	got := h.LinesToRestore()
	if len(got) != 1 || got[0] != "dom" {
		t.Fatalf("重复 remember 后应为 [dom]，实际 %v", got)
	}
}

// forgetLine 必须真的摘掉，且不影响其它线路。
func TestForgetLine(t *testing.T) {
	h := &Hub{}
	h.rememberLine("dom")
	h.rememberLine("intl")
	h.rememberLine("qoder")

	h.forgetLine("intl")
	got := h.LinesToRestore()
	want := []string{"dom", "qoder"}
	if len(got) != len(want) {
		t.Fatalf("forget 后应为 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forget 后应为 %v，实际 %v", want, got)
		}
	}

	// 摘掉不存在的线路是安全的空操作
	h.forgetLine("mimo")
	if len(h.LinesToRestore()) != 2 {
		t.Fatalf("forget 不存在的线路不应改变列表，实际 %v", h.LinesToRestore())
	}
}

// 关键回归：state.json 里的 last_lines 必须能读回来。
// 这条钉住「重启后线路不丢」这个用户可见行为。
func TestLastLinesSurviveStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// hubRoot 是包级变量、在包初始化时就解析好了，t.Setenv 改不动它。
	// 这里直接改这个变量，并在测试结束后还原，避免污染其它测试。
	oldRoot := hubRoot
	hubRoot = dir
	t.Cleanup(func() { hubRoot = oldRoot })

	h := &Hub{}
	h.rememberLine("dom")
	h.rememberLine("qoder")

	// 模拟进程重启：新建 Hub 从同一个 state.json 加载
	h2 := &Hub{}
	h2.loadState()
	got := h2.LinesToRestore()
	want := []string{"dom", "qoder"}
	if len(got) != len(want) {
		t.Fatalf("重启后应恢复 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("重启后应恢复 %v，实际 %v", want, got)
		}
	}
	// 顺带确认 state.json 真的被写出来了
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("state.json 未写出：%v", err)
	}
}

// 用户主动停掉的线路，重启后不该自己回来。
func TestStoppedLineNotRestored(t *testing.T) {
	dir := t.TempDir()
	// hubRoot 是包级变量、在包初始化时就解析好了，t.Setenv 改不动它。
	// 这里直接改这个变量，并在测试结束后还原，避免污染其它测试。
	oldRoot := hubRoot
	hubRoot = dir
	t.Cleanup(func() { hubRoot = oldRoot })

	h := &Hub{}
	h.rememberLine("dom")
	h.rememberLine("trae")
	h.forgetLine("trae") // 用户点了「停止」

	h2 := &Hub{}
	h2.loadState()
	got := h2.LinesToRestore()
	if len(got) != 1 || got[0] != "dom" {
		t.Fatalf("停掉的 trae 不该被恢复，实际 %v", got)
	}
}

// startLineByName 对未知名字要明确报错，不能静默成功。
func TestStartLineByNameRejectsUnknown(t *testing.T) {
	h := &Hub{}
	ok, msg := h.startLineByName("不存在的线路")
	if ok {
		t.Fatalf("未知线路不该返回成功")
	}
	if msg == "" {
		t.Fatalf("未知线路应给出说明")
	}
}
