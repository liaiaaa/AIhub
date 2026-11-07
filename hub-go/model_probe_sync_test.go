package main

// 「探测 = 一次模型更新」的回归测试（2026-11-07）。
//
// 用户补充语义：「跟着官方探测的模型来就行，如果官网 app 里面删除了某个
// 模型，那你也得删除掉，就可以理解为一次模型更新」。
//
// 测试盯住三件容易做错的事：
//   1. 默认模式必须整组镜像：官方新增的全加、官方没有的全删；
//   2. 只有 Official（官方目录条目）能构成保留集合，本地手工兜底不算；
//   3. 非权威清单、空清单、隐藏线路都绝不能触发删除。

import (
	"os"
	"path/filepath"
	"testing"
)

func withSyncTestAggList(t *testing.T, in []aggModel) {
	t.Helper()
	aggMu.Lock()
	saved := aggList
	aggList = append([]aggModel(nil), in...)
	aggMu.Unlock()
	t.Cleanup(func() {
		aggMu.Lock()
		aggList = saved
		aggMu.Unlock()
	})
}

func withNoHiddenLines(t *testing.T) {
	t.Helper()
	linesVisState.mu.Lock()
	savedSet, savedLoad := linesVisState.set, linesVisState.load
	linesVisState.set, linesVisState.load = map[string]bool{}, true
	linesVisState.mu.Unlock()
	t.Cleanup(func() {
		linesVisState.mu.Lock()
		linesVisState.set, linesVisState.load = savedSet, savedLoad
		linesVisState.mu.Unlock()
	})
}

func withTestHubRoot(t *testing.T) string {
	t.Helper()
	saved := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = saved })
	return dir
}

func TestSyncProbeModelsMirrorsOfficialCatalogByDefault(t *testing.T) {
	withNoHiddenLines(t)
	root := withTestHubRoot(t)
	withSyncTestAggList(t, []aggModel{
		{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"},
		{Line: "qoder", Slug: "qoder.Retired", Backend: "Retired"},
		{Line: "trae", Slug: "trae.deepseek-v4.1-flash", Backend: "deepseek-v4.1-flash"},
	})
	if err := os.WriteFile(filepath.Join(root, "agg_models.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}

	lines := []modelProbeLine{{
		Line: "qoder", Authoritative: true,
		Models: []modelProbeItem{
			{ID: "qfmodel", Display: "Qwen3.8-Flash", Ratio: "0.1x", Official: true},
			{ID: "lmodel", Display: "QoDuck", Ratio: "免费", Free: true, Official: true},
			{ID: "manual-only", Display: "Manual", Official: false},
		},
	}}
	res := syncProbeModels(lines, probeSyncOptions{})

	if !containsString(res.Added, "qoder.QoDuck") {
		t.Fatalf("官方新增的免费模型必须写进目录，得到 %v", res.Added)
	}
	var hasFlash bool
	for _, m := range aggList {
		if m.Slug == "qoder.Qwen3.8-Flash" {
			hasFlash = true
		}
		if m.Line == "qoder" && (m.Backend == "manual-only" || m.Backend == "Retired") {
			t.Fatalf("官方清单里没有的条目不该留在目录里：%v", aggList)
		}
	}
	if !hasFlash {
		t.Fatalf("官方仍在的 0.1x 模型必须保留：%v", aggList)
	}
	if containsString(res.Added, "qoder.Manual") || containsString(res.Added, "qoder.manual-only") {
		t.Fatalf("非官方兜底条目不该被当成官方模型写入，得到 %v", res.Added)
	}
	if !containsString(res.Removed, "qoder.Retired") {
		t.Fatalf("官方清单里没有的旧条目必须删除，得到 %v", res.Removed)
	}
	if containsString(res.Removed, "trae.deepseek-v4.1-flash") {
		t.Fatalf("未参与探测的线路不该被误删，得到 %v", res.Removed)
	}
	if len(res.Free) != 1 || res.Free[0] != "qoder.QoDuck" {
		t.Fatalf("免费汇总必须只报 QoDuck，得到 %v", res.Free)
	}
	if res.Backup == "" {
		t.Fatal("发生删除前必须备份 agg_models.json")
	}
	if _, err := os.Stat(filepath.Join(root, res.Backup)); err != nil {
		t.Fatalf("备份文件必须落在当前数据根：%v", err)
	}
	for _, m := range aggList {
		if m.Line == "qoder" && m.Backend == "manual-only" {
			t.Fatalf("本地兜底条目不该留在目录里：%v", aggList)
		}
	}
}

func TestSyncProbeModelsSupplementNeverRemoves(t *testing.T) {
	withNoHiddenLines(t)
	withTestHubRoot(t)
	withSyncTestAggList(t, []aggModel{
		{Line: "qoder", Slug: "qoder.Retired", Backend: "Retired"},
	})
	res := syncProbeModels([]modelProbeLine{{
		Line: "qoder", Authoritative: true,
		Models: []modelProbeItem{{ID: "lmodel", Display: "QoDuck", Free: true, New: true, Official: true}},
	}}, probeSyncOptions{Mode: "supplement"})

	if !containsString(res.Added, "qoder.QoDuck") {
		t.Fatalf("supplement 仍应补新/免费模型，得到 %v", res.Added)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("supplement 必须只增不删，得到 %v", res.Removed)
	}
}

func TestSyncProbeModelsNonAuthoritativeNeverRemoves(t *testing.T) {
	withNoHiddenLines(t)
	withTestHubRoot(t)
	withSyncTestAggList(t, []aggModel{
		{Line: "qoder", Slug: "qoder.Retired", Backend: "Retired"},
	})
	res := syncProbeModels([]modelProbeLine{{
		Line: "qoder", Authoritative: false,
		Models: []modelProbeItem{{ID: "lmodel", Display: "QoDuck", Free: true, Official: true}},
	}}, probeSyncOptions{})

	if len(res.Removed) != 0 {
		t.Fatalf("非权威清单绝不删除，得到 %v", res.Removed)
	}
	if !containsString(res.Skipped, "qoder") {
		t.Fatalf("非权威线路要写进 Skipped，得到 %v", res.Skipped)
	}
}

func TestSyncProbeModelsEmptyLineSkipped(t *testing.T) {
	withNoHiddenLines(t)
	withTestHubRoot(t)
	withSyncTestAggList(t, []aggModel{
		{Line: "teleagent", Slug: "teleagent.chat-pro", Backend: "chat-pro"},
	})
	res := syncProbeModels([]modelProbeLine{{Line: "teleagent", Authoritative: true}}, probeSyncOptions{})

	if len(res.Removed) != 0 {
		t.Fatalf("空清单不能清空目录，得到 %v", res.Removed)
	}
	if !containsString(res.Skipped, "teleagent") {
		t.Fatalf("空清单也要写进 Skipped，得到 %v", res.Skipped)
	}
}

func TestSyncProbeModelsQoderAliasesKeepOldDisplayName(t *testing.T) {
	withNoHiddenLines(t)
	withTestHubRoot(t)
	withSyncTestAggList(t, []aggModel{
		{Line: "qoder", Slug: "qoder.Kimi-K3", Backend: "Kimi-K3"},
	})
	res := syncProbeModels([]modelProbeLine{{
		Line: "qoder", Authoritative: true,
		Models: []modelProbeItem{{ID: "kmodel_latest", Display: "Kimi-K3.1-Pro", Official: true}},
	}}, probeSyncOptions{})

	if containsString(res.Removed, "qoder.Kimi-K3") {
		t.Fatalf("旧显示名是官方别名，不该被当官方下架删掉，得到 %v", res.Removed)
	}
	if !containsString(res.Added, "qoder.Kimi-K3.1-Pro") {
		t.Fatalf("官方新显示名应同步进目录，得到 %v", res.Added)
	}
}
