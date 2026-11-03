package main

// 2026-11-03：线路显隐（隐藏 = 不显示 / 不能启动 / 不进聚合，配置与凭据不动）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// resetVisibility 清掉内存缓存，让下一次 hiddenLines() 重新读盘。
// 测试之间必须隔离，否则前一个用例的隐藏状态会带到后一个。
func resetVisibility(t *testing.T) {
	t.Helper()
	linesVisState.mu.Lock()
	linesVisState.set = nil
	linesVisState.load = false
	linesVisState.mu.Unlock()
	t.Cleanup(func() {
		linesVisState.mu.Lock()
		linesVisState.set = nil
		linesVisState.load = false
		linesVisState.mu.Unlock()
		_ = os.Remove(dataPath(linesConfigFile))
	})
}

func TestLineVisibilityRoundTrip(t *testing.T) {
	resetVisibility(t)
	if lineHidden("mimo") {
		t.Fatal("初始状态 mimo 不应被隐藏")
	}
	if err := setLineHidden("mimo", true); err != nil {
		t.Fatalf("隐藏失败：%v", err)
	}
	if !lineHidden("mimo") {
		t.Error("隐藏后 lineHidden(mimo) 应为 true")
	}
	for _, k := range visibleLineKeys() {
		if k == "mimo" {
			t.Error("隐藏后 visibleLineKeys 不应包含 mimo")
		}
	}
	// 落盘内容可读且只记 key
	b, err := os.ReadFile(dataPath(linesConfigFile))
	if err != nil {
		t.Fatalf("lines.json 没写出来：%v", err)
	}
	var cfg linesConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("lines.json 不是合法 JSON：%s", b)
	}
	if len(cfg.Hidden) != 1 || cfg.Hidden[0] != "mimo" {
		t.Errorf("lines.json 内容 = %v，期望 [mimo]", cfg.Hidden)
	}
	// 恢复
	if err := setLineHidden("mimo", false); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if lineHidden("mimo") {
		t.Error("恢复后不该还是隐藏")
	}
}

func TestLineVisibilityRejectsUnknownKey(t *testing.T) {
	resetVisibility(t)
	if err := setLineHidden("nope", true); err == nil {
		t.Error("未知线路应当报错")
	}
}

func TestHiddenLineIgnoredWhenConfigHasUnknown(t *testing.T) {
	resetVisibility(t)
	// 用户手改配置写了不存在的线路：应被忽略，不影响启动
	path := dataPath(linesConfigFile)
	if err := os.WriteFile(path, []byte(`{"hidden":["mimo","ghost"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if lineHidden("ghost") {
		t.Error("未知线路不该被当成隐藏（否则配置写错会影响运行）")
	}
	if !lineHidden("mimo") {
		t.Error("已知线路 mimo 应被隐藏")
	}
}

func TestHiddenLineNotSelectableForAgg(t *testing.T) {
	resetVisibility(t)
	// 用 trae：它是参与聚合的线路（mimo 本来就不进聚合，不适合当这个用例的样本）
	withAggList(t, []aggModel{
		{Slug: "trae.deepseek-v4.1-flash", Line: "trae", Backend: "deepseek-v4.1-flash"},
		{Slug: "qoder.qfmodel", Line: "qoder", Backend: "qfmodel"},
	})
	if !aggSelectableLine("trae") {
		t.Fatal("隐藏前 trae 应可选")
	}
	if err := setLineHidden("trae", true); err != nil {
		t.Fatal(err)
	}
	if aggSelectableLine("trae") {
		t.Error("隐藏后 trae 不应可选（不能再加进聚合白名单）")
	}
	for _, m := range aggSnapshot() {
		if m.Line == "trae" {
			t.Error("隐藏后 trae 的模型不应出现在聚合快照里")
		}
	}
	h := &Hub{}
	out := h.aggOpenAIModels()
	for _, raw := range out["data"].([]map[string]any) {
		if strings.HasPrefix(raw["id"].(string), "trae.") {
			t.Error("/v1/models 不该再列出隐藏线路的模型")
		}
	}
}

func TestLineStartRejectedWhenHidden(t *testing.T) {
	resetVisibility(t)
	if err := setLineHidden("mimo", true); err != nil {
		t.Fatal(err)
	}
	h := &Hub{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/mimo/start", strings.NewReader("{}"))
	h.apiLineStart("mimo")(w, req)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%s", w.Body.String())
	}
	if resp["ok"] != false {
		t.Errorf("隐藏线路的启动应被拒绝，实际 %v", resp)
	}
	if msg, _ := resp["msg"].(string); !strings.Contains(msg, "已隐藏") {
		t.Errorf("提示应说明已隐藏，实际 %q", msg)
	}
}

func TestApiLinesListsVisibility(t *testing.T) {
	resetVisibility(t)
	if err := setLineHidden("oc", true); err != nil {
		t.Fatal(err)
	}
	h := &Hub{}
	w := httptest.NewRecorder()
	h.apiLines(w, httptest.NewRequest(http.MethodGet, "/api/lines", nil))
	var resp struct {
		OK    bool             `json:"ok"`
		Lines []map[string]any `json:"lines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%s", w.Body.String())
	}
	if !resp.OK || len(resp.Lines) != len(lineSpecs) {
		t.Fatalf("应返回全部 %d 条线路，实际 %d（ok=%v）", len(lineSpecs), len(resp.Lines), resp.OK)
	}
	found := false
	for _, l := range resp.Lines {
		if l["key"] == "oc" {
			found = true
			if l["hidden"] != true {
				t.Errorf("oc 的 hidden 应为 true，实际 %v", l["hidden"])
			}
		}
	}
	if !found {
		t.Error("响应里没有 oc")
	}
}
