package main

// agg_models.json 原子写回归测试（2026-11-07）。
//
// 旧实现：`os.WriteFile(dataPath("agg_models.json"), b, 0644)` —— 错误被完全吞掉，
// 写到一半被强杀/磁盘满会留半截 JSON，而 aggLoad 是静默回落到默认清单的：
// 用户表现为「我手选的目录模型全没了」且日志里查不到任何原因。
// state.json 早在 2026-10-31 就改成 tmp+rename+记日志，这里补齐。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withAggSaveTestRoot(t *testing.T) string {
	t.Helper()
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	return hubRoot
}

func TestAggSaveLockedWritesFullJSONAndNoTempLeftover(t *testing.T) {
	dir := withAggSaveTestRoot(t)
	aggMu.Lock()
	old := aggList
	aggList = []aggModel{
		{Line: "qoder", Slug: "qoder.QoDuck", Backend: "QoDuck", Display: "QoDuck（Qoder）"},
	}
	aggSaveLocked()
	aggMu.Unlock()
	t.Cleanup(func() {
		aggMu.Lock()
		aggList = old
		aggMu.Unlock()
	})

	b, err := os.ReadFile(filepath.Join(dir, "agg_models.json"))
	if err != nil {
		t.Fatalf("agg_models.json 应被写出：%v", err)
	}
	var got []aggModel
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("写出的必须是完整合法 JSON（不能是半截）：%v\n内容=%q", err, b)
	}
	if len(got) != 1 || got[0].Slug != "qoder.QoDuck" {
		t.Fatalf("内容不对：%+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "agg_models.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("成功后不应残留 .tmp 文件")
	}
}

func TestAggSaveLockedLogsWriteFailure(t *testing.T) {
	dir := withAggSaveTestRoot(t)
	// 用同名目录占住目标路径：写 tmp 成功、rename 必然失败。
	// 旧实现 os.WriteFile 会失败且吞掉错误 → hub.log 里没有任何线索（RED）。
	if err := os.MkdirAll(filepath.Join(dir, "agg_models.json"), 0755); err != nil {
		t.Fatal(err)
	}
	aggMu.Lock()
	old := aggList
	aggList = []aggModel{{Line: "dom", Slug: "dom.x", Backend: "x"}}
	aggSaveLocked()
	aggMu.Unlock()
	t.Cleanup(func() {
		aggMu.Lock()
		aggList = old
		aggMu.Unlock()
	})

	logs, _ := os.ReadFile(filepath.Join(dir, "hub.log"))
	if !strings.Contains(string(logs), "agg_models.json") {
		t.Fatal("写失败必须记进 hub.log（否则用户查不到目录丢失的原因）")
	}
	if _, err := os.Stat(filepath.Join(dir, "agg_models.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("rename 失败后必须清理临时文件")
	}
}
