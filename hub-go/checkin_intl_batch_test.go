package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runIntlCheckin 的核心承诺：**一次调用覆盖全部国际凭证**，且不碰国内/Trae。
// （2026-10-12 用户要求：原先混在一键签到里只能覆盖当前选中的凭证，
// 要覆盖多个账号得反复切凭证+点签到，每次都顺带重打国内/Trae。）

func seedIntlCred(t *testing.T, name, uid string) {
	t.Helper()
	dir := dataPath(name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cred := `{"auth":{"accessToken":"a.b.c","domain":"www.workbuddy.ai","expiresAt":9999999999999},` +
		`"account":{"uid":"` + uid + `","nickname":"` + name + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop-ai.info"), []byte(cred), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRunIntlCheckinCoversAllCreds(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	seedIntlCred(t, "<凭证目录A>", "uid-li1")
	seedIntlCred(t, "<凭证目录B>", "uid-li2")
	seedIntlCred(t, "<凭证目录D>", "uid-gou")

	var mu sync.Mutex
	seen := map[string]int{} // uid -> 上报次数
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var arr []map[string]any
		json.NewDecoder(r.Body).Decode(&arr)
		if len(arr) > 0 {
			if uid, ok := arr[0]["userId"].(string); ok {
				mu.Lock()
				seen[uid]++
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer up.Close()

	oldURL := intlReportURL
	intlReportURL = up.URL + "/v2/report"
	defer func() { intlReportURL = oldURL }()

	h := &Hub{}
	ok, msg := runIntlCheckin(h)
	if !ok {
		t.Fatalf("应全部成功：%s", msg)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, uid := range []string{"uid-li1", "uid-li2", "uid-gou"} {
		if seen[uid] != 1 {
			t.Fatalf("uid %s 应被上报恰好 1 次，实际 %d（seen=%v）", uid, seen[uid], seen)
		}
	}
	// 结果文案应逐个列出账号
	for _, n := range []string{"<凭证目录A>", "<凭证目录B>", "<凭证目录D>"} {
		if !strings.Contains(msg, n) {
			t.Fatalf("结果文案应含 %s：%s", n, msg)
		}
	}
	t.Logf("文案：%s", msg)
}

// 没有任何国际凭证时，应明确失败而不是静默成功
func TestRunIntlCheckinNoCreds(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	h := &Hub{}
	ok, msg := runIntlCheckin(h)
	if ok {
		t.Fatalf("无凭证不该报成功：%s", msg)
	}
	t.Logf("文案：%s", msg)
}

// 部分凭证失败时：整体 ok=false，但成功的那几个仍要出现在文案里
func TestRunIntlCheckinPartialFailure(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	seedIntlCred(t, "好凭证", "uid-good")
	// 坏凭证：uid 缺失（服务端会静默丢弃，这里本地就该拦下）
	dir := dataPath("坏凭证")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "workbuddy-desktop-ai.info"),
		[]byte(`{"auth":{"accessToken":"a.b.c","domain":"www.workbuddy.ai","expiresAt":9999999999999},"account":{}}`), 0600)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer up.Close()
	oldURL := intlReportURL
	intlReportURL = up.URL + "/v2/report"
	defer func() { intlReportURL = oldURL }()

	h := &Hub{}
	ok, msg := runIntlCheckin(h)
	if ok {
		t.Fatalf("有一个失败时整体不该报成功：%s", msg)
	}
	if !strings.Contains(msg, "好凭证") {
		t.Fatalf("成功的那项要出现在文案里：%s", msg)
	}
	if !strings.Contains(msg, "坏凭证") {
		t.Fatalf("失败的那项也要出现（让用户知道谁没成）：%s", msg)
	}
	t.Logf("文案：%s", msg)
}

// 一键签到不再包含国际：runCheckin 只报国内 + Trae
func TestRunCheckinExcludesIntl(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	// 桩掉国际上报，记录是否被调用
	var called bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer up.Close()
	oldURL := intlReportURL
	intlReportURL = up.URL + "/v2/report"
	defer func() { intlReportURL = oldURL }()

	seedIntlCred(t, "<凭证目录A>", "uid-x")

	h := &Hub{}
	runCheckin(h) // 国内/Trae 会失败（无凭据），但我们只关心国际有没有被顺手打
	if called {
		t.Fatalf("一键签到不该再触发国际上报（应走单独的 /api/intl/checkin）")
	}
}
