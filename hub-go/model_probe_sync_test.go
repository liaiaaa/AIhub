package main

// 「探测并更新 = 更新线路模型库，绝不动用户的目录模型」（2026-11-07 用户纠正）。
//
// 用户原话：「我想要的探测并更新，是更新数据库里面的模型并不是更新我
// 【目录模型（客户端可见列表）】这个里面的，聚合线路中的模型我自己手动添加就行」。
//
// 上一版把官方清单整组镜像进 agg_models.json，把用户手动维护的目录冲掉了。
// 这一版只更新各线路自己的模型库（qoder 目录热替换+落盘、dom/intl 的
// wb_catalog_*.json 由探测本身刷新），agg_models.json 一个字节都不许动。

import (
	"errors"
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

func withFakeQoderFetch(t *testing.T, entries []qoderModelEntry, err error) {
	t.Helper()
	old := probeQoderFetch
	probeQoderFetch = func() ([]qoderModelEntry, error) { return entries, err }
	t.Cleanup(func() { probeQoderFetch = old })
}

func TestProbeSyncNeverTouchesAggList(t *testing.T) {
	withNoHiddenLines(t)
	root := withTestHubRoot(t)
	before := []aggModel{
		{Line: "qoder", Slug: "qoder.Qwen3.8-Flash", Backend: "Qwen3.8-Flash"},
		{Line: "dom", Slug: "dom.glm-5.3-flash", Backend: "glm-5.3-flash"},
	}
	withSyncTestAggList(t, before)
	// 目录文件也放一份，验证连盘上文件都不动。
	dirBytes := []byte(`[{"line":"qoder","slug":"qoder.Qwen3.8-Flash","backend":"Qwen3.8-Flash"}]`)
	if err := os.WriteFile(filepath.Join(root, "agg_models.json"), dirBytes, 0600); err != nil {
		t.Fatal(err)
	}
	withFakeQoderFetch(t, []qoderModelEntry{
		{Key: "lmodel", DisplayName: "QoDuck", PriceFactor: 0, IsFree: true},
	}, nil)

	h := &Hub{qoder: NewQoderProxy()}
	_, _ = syncModelCatalogs(h, probeSyncOptions{})

	aggMu.Lock()
	got := append([]aggModel(nil), aggList...)
	aggMu.Unlock()
	if len(got) != len(before) {
		t.Fatalf("目录模型绝不该被同步改动：before=%d after=%d（%+v）", len(before), len(got), got)
	}
	for i := range before {
		if got[i].Slug != before[i].Slug {
			t.Fatalf("目录模型顺序/内容变了：%+v → %+v", before, got)
		}
	}
	after, err := os.ReadFile(filepath.Join(root, "agg_models.json"))
	if err != nil || string(after) != string(dirBytes) {
		t.Fatalf("agg_models.json 磁盘文件也不该动：%q → %q（err=%v）", dirBytes, after, err)
	}
}

func TestProbeSyncUpdatesQoderCatalogDB(t *testing.T) {
	withNoHiddenLines(t)
	root := withTestHubRoot(t)
	withSyncTestAggList(t, nil)
	withFakeQoderFetch(t, []qoderModelEntry{
		{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", PriceFactor: 0.1, IsFree: true},
		{Key: "lmodel", DisplayName: "QoDuck", PriceFactor: 0, IsFree: true},
	}, nil)

	p := NewQoderProxy()
	// 预置一份「旧模型库」（不含 QoDuck、含一个将被官方下架的旧模型），
	// 模拟上次更新落盘的清单；内置快照已含 QoDuck，不能拿它当基线。
	p.setCatalog([]qoderModelEntry{
		{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", PriceFactor: 0.1},
		{Key: "retired", DisplayName: "Retired-Model", PriceFactor: 0.5},
	})
	h := &Hub{qoder: p}
	res, _ := syncModelCatalogs(h, probeSyncOptions{})

	// 1. 运行中的线路目录被热替换成官方清单
	snap := p.catalogSnapshot()
	if len(snap) != 2 {
		t.Fatalf("线路模型库应被官方清单替换（2 个），得到 %d：%+v", len(snap), snap)
	}
	// 2. 落盘，重启后也能用
	if _, err := os.Stat(filepath.Join(root, "qoder_catalog.json")); err != nil {
		t.Fatalf("官方清单必须落盘成 qoder_catalog.json：%v", err)
	}
	loaded := loadQoderCatalogDB()
	if len(loaded) != 2 {
		t.Fatalf("落盘的清单要能读回（2 个），得到 %d", len(loaded))
	}
	// 3. 回执：新增 QoDuck、下架内置快照里的旧模型、免费只报 QoDuck
	if !containsString(res.Added, "qoder.QoDuck") {
		t.Fatalf("新增要报 QoDuck，得到 %v", res.Added)
	}
	if len(res.Removed) == 0 {
		t.Fatal("内置快照里被官方下架的旧模型要报 removed")
	}
	if !containsString(res.Updated, "qoder") {
		t.Fatalf("qoder 要写进 Updated，得到 %v", res.Updated)
	}
	if !containsString(res.Free, "qoder.QoDuck") {
		t.Fatalf("免费汇总必须有 QoDuck，得到 %v", res.Free)
	}
	if containsString(res.Free, "qoder.Qwen3.8-Flash") {
		t.Fatalf("qfmodel 是 0.1x，绝不能报成免费，得到 %v", res.Free)
	}
}

func TestProbeSyncFetchFailureKeepsDB(t *testing.T) {
	withNoHiddenLines(t)
	root := withTestHubRoot(t)
	withSyncTestAggList(t, nil)
	withFakeQoderFetch(t, nil, errors.New("网络不通"))

	p := NewQoderProxy()
	before := len(p.catalogSnapshot())
	h := &Hub{qoder: p}
	res, _ := syncModelCatalogs(h, probeSyncOptions{})

	if len(p.catalogSnapshot()) != before {
		t.Fatal("抓取失败绝不能动现有模型库")
	}
	if _, err := os.Stat(filepath.Join(root, "qoder_catalog.json")); !os.IsNotExist(err) {
		t.Fatal("抓取失败不该写落盘文件")
	}
	if !containsString(res.Skipped, "qoder") {
		t.Fatalf("抓取失败要写进 Skipped，得到 %v", res.Skipped)
	}
}

func TestQoderCatalogDBRoundTrip(t *testing.T) {
	withTestHubRoot(t)
	if got := loadQoderCatalogDB(); got != nil {
		t.Fatalf("没有落盘文件时应返回 nil，得到 %+v", got)
	}
	want := []qoderModelEntry{
		{Key: "lmodel", DisplayName: "QoDuck", PriceFactor: 0, IsFree: true},
		{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", PriceFactor: 0.1},
	}
	if err := saveQoderCatalogDB(want); err != nil {
		t.Fatal(err)
	}
	got := loadQoderCatalogDB()
	if len(got) != 2 || got[0].Key != "lmodel" || got[1].PriceFactor != 0.1 {
		t.Fatalf("落盘读回不一致：%+v", got)
	}
}
