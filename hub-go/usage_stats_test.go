package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// feedTap 把一段响应体按行逐块喂给取样器（贴近 responsesStream 的整帧写出）。
func feedTap(t *testing.T, body string) *usageTap {
	t.Helper()
	rec := httptest.NewRecorder()
	tap := &usageTap{w: rec}
	br := bufio.NewReader(strings.NewReader(body))
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if _, werr := tap.Write([]byte(line)); werr != nil {
				t.Fatalf("write: %v", werr)
			}
		}
		if err != nil {
			break
		}
	}
	return tap
}

func check(t *testing.T, tap *usageTap, want usageRec) {
	t.Helper()
	in, out, cached, reason, total := usageFrom(tap.usage)
	got := usageRec{In: in, Out: out, Cached: cached, Reason: reason, Total: total}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageTapChatFrame(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[],"usage":{"prompt_tokens":1234,"completion_tokens":56,"total_tokens":1290,"prompt_tokens_details":{"cached_tokens":900},"completion_tokens_details":{"reasoning_tokens":20}}}

data: [DONE]

`
	check(t, feedTap(t, body), usageRec{In: 1234, Out: 56, Cached: 900, Reason: 20, Total: 1290})
}

func TestUsageTapResponsesCompleted(t *testing.T) {
	body := `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":40000,"output_tokens":300,"total_tokens":40300,"input_tokens_details":{"cached_tokens":39000}}}}

`
	check(t, feedTap(t, body), usageRec{In: 40000, Out: 300, Cached: 39000, Reason: 0, Total: 40300})
}

// 上游不带 details 时 total 要能兜底，且不被 usage_hint 这类相似键名误伤。
func TestUsageTapNoDetailsAndLookalikeKeys(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"x"}}],"usage_hint":"noise"}

data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}

`
	check(t, feedTap(t, body), usageRec{In: 7, Out: 3, Cached: 0, Reason: 0, Total: 10})
}

func TestUsageTapMissingUsageStaysZero(t *testing.T) {
	tap := feedTap(t, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	if len(tap.usage) != 0 {
		t.Fatalf("expected no usage captured, got %v", tap.usage)
	}
	in, out, _, _, total := usageFrom(tap.usage)
	if in+out+total != 0 {
		t.Fatalf("expected zeros, got %d %d %d", in, out, total)
	}
}

// usage 嵌在大对象里时，括号配对不能被字符串内容里的花括号和转义引号带偏。
func TestUsageTapBracesInText(t *testing.T) {
	body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7},\"note\":\"text with } and \\\" escapes\"}\n\n"
	check(t, feedTap(t, body), usageRec{In: 5, Out: 2, Total: 7})
}

func TestUsageTapKeepsStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	tap := &usageTap{w: rec}
	tap.WriteHeader(http.StatusServiceUnavailable)
	tap.Write([]byte(`{"error":{"message":"line off"}}`))
	if tap.code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", tap.code)
	}
}

// hubRoot 在 init 期就解析好了，测试直接换包级变量并复位增量缓存，
// 否则会写进真实数据根、污染正式统计。
func isolateUsageRoot(t *testing.T) {
	t.Helper()
	old := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = old })
	usageCacheMu.Lock()
	usageCacheRows, usageCacheOff, usageCacheSize = nil, 0, 0
	usageCacheMu.Unlock()
}

func TestUsageReportAggregatesByDayAndModel(t *testing.T) {
	isolateUsageRoot(t)
	// 日期必须相对「今天」算，不能写死常量：usageReport(days) 的窗口是
	// time.Now() 起算的（cutoff = 今天 - days + 1），写死日期的用例过一天就会
	// 被窗口全部过滤掉，变成「时间炸弹」——2026-10-12 实测踩到，
	// 用例在 10-11 当天通过、隔天必红，且与产品代码无关。
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	rows := []usageRec{
		{Date: yesterday, Line: "intl", Slug: "intl.deepseek-v4.1-flash", In: 100, Out: 10, Total: 110, Code: 200},
		{Date: yesterday, Line: "intl", Slug: "intl.deepseek-v4.1-flash", In: 200, Out: 20, Cached: 150, Total: 220, Code: 500},
		{Date: today, Line: "dom", Slug: "dom.glm-5.3-flash", In: 50, Out: 5, Total: 55, Code: 200},
	}
	var sb strings.Builder
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(dataPath(usageFile), []byte(sb.String()), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep := usageReport(0)
	if rep["ok"] != true {
		t.Fatalf("report not ok: %v", rep)
	}
	sum := rep["summary"].(usageAgg)
	if sum.Reqs != 3 || sum.Total != 385 || sum.Fail != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if got := len(rep["models"].([]map[string]any)); got != 2 {
		t.Fatalf("models = %d, want 2", got)
	}
	days := rep["days"].([]map[string]any)
	if len(days) != 2 || days[0]["date"] != today {
		t.Fatalf("days = %+v", days)
	}
	if rep["from"] != yesterday || rep["to"] != today {
		t.Fatalf("span = %v → %v", rep["from"], rep["to"])
	}
	lines := rep["lines"].([]map[string]any)
	if len(lines) != 2 || lines[0]["line"] != "dom" || lines[0]["line_cn"] != "国内" {
		t.Fatalf("lines = %+v", lines)
	}
	// days=1 只看今天，昨天的记录要被判掉（今天 1 条、昨天 2 条）
	if r := usageReport(1); r["summary"].(usageAgg).Reqs != 1 {
		t.Fatalf("window filter broken: %+v", r["summary"])
	}
}

// 尾部只写了一半的行不能被吞掉：补齐之后增量读要能把它读出来。
func TestUsageRowsIncrementalAcrossHalfLine(t *testing.T) {
	isolateUsageRoot(t)
	path := dataPath(usageFile)
	full := `{"ts":1,"d":"2026-10-11","line":"intl","slug":"intl.a","code":200,"in":10,"out":1,"total":11}` + "\n"
	half := `{"ts":2,"d":"2026-10-11","line":"intl","slug":"intl.b","code":200,"in":20,"out":2,"to`
	tail := `tal":22}` + "\n"

	if err := os.WriteFile(path, []byte(full+half), 0644); err != nil {
		t.Fatal(err)
	}
	rows, err := usageRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Slug != "intl.a" {
		t.Fatalf("rows after half-line = %+v", rows)
	}

	if err := os.WriteFile(path, []byte(full+half+tail), 0644); err != nil {
		t.Fatal(err)
	}
	rows2, err := usageRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 2 || rows2[1].Slug != "intl.b" || rows2[1].In != 20 || rows2[1].Total != 22 {
		t.Fatalf("rows after completion = %+v", rows2)
	}
}
