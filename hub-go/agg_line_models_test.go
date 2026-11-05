package main

// 「探测到但加不进目录」这个死角的回归测试（2026-11-05）。
//
// 用户报：探测新模型能探到 hy4，但打开「目录模型」那边没办法添加这个模型。
// 根因：两条路径数据源不同 —— 探测打上游目录（线路没启动也有结果），
// 添加下拉只读线路自己的 /v1/models（线路没跑就报「请先启动它」）。
// 修法：线路没跑时回落上游目录；线路在跑时把上游多出来的 id 并进来。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func withFakeProbe(t *testing.T, fn func(*Hub, string) ([]string, map[string]string)) {
	t.Helper()
	old := probeLineModelIDs
	probeLineModelIDs = fn
	t.Cleanup(func() { probeLineModelIDs = old })
}

func decodeLineModels(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, truncate(string(body), 200))
	}
	return out
}

func TestAggLineModelsFallsBackToUpstreamWhenStopped(t *testing.T) {
	withFakeProbe(t, func(_ *Hub, line string) ([]string, map[string]string) {
		if line != "dom" {
			return nil, nil
		}
		return []string{"hy4free", "hy3", "glm-5.3"}, map[string]string{"hy4free": "Hy4"}
	})
	h := &Hub{}
	req := httptest.NewRequest(http.MethodGet, "/api/agg/line_models?line=dom", nil)
	rec := httptest.NewRecorder()
	h.apiAggLineModels(rec, req)

	got := decodeLineModels(t, rec.Body.Bytes())
	if got["ok"] != true {
		t.Fatalf("线路未启动时也该返回 ok=true（回落上游），得到 %v（msg=%v）", got["ok"], got["msg"])
	}
	models, _ := got["models"].([]any)
	var ids []string
	for _, m := range models {
		ids = append(ids, m.(string))
	}
	if !stringInSlice(ids, "hy4free") {
		t.Fatalf("上游探到的新模型必须出现在可添加列表里，得到 %v", ids)
	}
	if got["source"] != "upstream" {
		t.Errorf("source 应是 upstream，得到 %v", got["source"])
	}
	if got["note"] == nil || got["note"] == "" {
		t.Error("回落上游时要给前端一句解释，别让列表来路不明")
	}
	disp, _ := got["display"].(map[string]any)
	if disp == nil || disp["hy4free"] != "Hy4" {
		t.Errorf("官方显示名应带过去，得到 %v", got["display"])
	}
}

func TestAggLineModelsStoppedAndNoUpstreamStillSaysWhy(t *testing.T) {
	withFakeProbe(t, func(*Hub, string) ([]string, map[string]string) { return nil, nil })
	h := &Hub{}
	rec := httptest.NewRecorder()
	h.apiAggLineModels(rec, httptest.NewRequest(http.MethodGet, "/api/agg/line_models?line=dom", nil))

	got := decodeLineModels(t, rec.Body.Bytes())
	if got["ok"] == true {
		t.Fatal("上游也没取到模型时不该假称成功")
	}
	if got["msg"] == nil || got["msg"] == "" {
		t.Fatal("必须给原因")
	}
}

func TestAggLineModelsUnknownLine(t *testing.T) {
	h := &Hub{}
	rec := httptest.NewRecorder()
	h.apiAggLineModels(rec, httptest.NewRequest(http.MethodGet, "/api/agg/line_models?line=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知线路应 400，得到 %d", rec.Code)
	}
}

func TestMergeUniqueAppendsOnlyNew(t *testing.T) {
	base := []string{"glm-5.3", "hy3"}
	extra := []string{"hy3", "hy4free", "", "kimi-k3"}
	got, appended := mergeUnique(base, extra)
	if !appended {
		t.Fatal("有新项时 appended 应为 true")
	}
	want := []string{"glm-5.3", "hy3", "hy4free", "kimi-k3"}
	if len(got) != len(want) {
		t.Fatalf("合并结果 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项应是 %s，得到 %v（全部 %v）", i, want[i], got[i], got)
		}
	}
	// base 不能被就地改坏（append 到共享底层数组是常见坑）
	if len(base) != 2 || base[1] != "hy3" {
		t.Fatalf("原切片被改了：%v", base)
	}
	// 全重复时不该报「追加过」
	if _, appended := mergeUnique([]string{"a"}, []string{"a"}); appended {
		t.Error("没有新项时 appended 应为 false")
	}
}

func TestAggHasProbeAddPathForStoppedLine(t *testing.T) {
	// 目录添加本身不要求线路在跑：aggAdd 只校验线路可选 + 模型名非空。
	// 这条测试钉住「探测 → 加入」闭环在后端是通的（前端一行一个按钮走的就是它）。
	oldRoot := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = oldRoot }()

	_, added, msg := aggAdd("dom", "hy4free")
	if !added {
		t.Fatalf("加入应成功，msg=%s", msg)
	}
	if _, again, msg2 := aggAdd("dom", "hy4free"); again {
		t.Fatalf("重复加入不该再算新增，msg=%s", msg2)
	}
}
