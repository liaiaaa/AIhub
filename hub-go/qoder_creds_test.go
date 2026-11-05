package main

// Qoder 多凭证看板的测试（2026-11-05）。
//
// UI 改成和 WorkBuddy 多凭证同一套卡片后，后端必须给出同样的字段语义：
// ok/state/selected/remain/total。这里用本地假上游覆盖 qoderCredsUsageURL，
// 不打真网络。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeQoderUsageServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := qoderCredsUsageURL
	qoderCredsUsageURL = srv.URL + "/sash/api/v2/me/usage"
	t.Cleanup(func() { qoderCredsUsageURL = old })
	return srv.URL
}

func TestQoderCredsStatusShowsSnapshots(t *testing.T) {
	appData := fakeQoderEnv(t)
	fakeQoderUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"userId":"u-1","userQuota":{"remaining":1200,"total":3000}}`))
	})

	writeFakeQoderCred(t, appData, "uid-a", "小号A", "tok-a")
	if _, err := qoderVaultSave("小号A"); err != nil {
		t.Fatal(err)
	}
	writeFakeQoderCred(t, appData, "uid-b", "小号B", "tok-b")
	if _, err := qoderVaultSave("小号B"); err != nil {
		t.Fatal(err)
	}
	// 当前登录 = 小号B
	qoderInvalidateCredsCache()
	list := qoderCredsStatus(true)
	if len(list) != 2 {
		t.Fatalf("应有 2 份凭证，得到 %+v", list)
	}
	var selA, selB bool
	for _, c := range list {
		if c.Name == "小号B" {
			selB = c.Selected
		}
		if c.Name == "小号A" {
			selA = c.Selected
		}
		if !c.OK || c.State != "可用" {
			t.Errorf("%s 应可用：%+v", c.Name, c)
		}
		if c.Remain != 1200 || c.Total != 3000 {
			t.Errorf("%s 余额不对：%v/%v", c.Name, c.Remain, c.Total)
		}
	}
	if !selB || selA {
		t.Fatalf("选中项应只有小号B：%+v", list)
	}
}

func TestQoderCredsStatusHandlesDeadTokens(t *testing.T) {
	appData := fakeQoderEnv(t)
	fakeQoderUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	writeFakeQoderCred(t, appData, "uid-a", "过期号", "tok-a")
	if _, err := qoderVaultSave("过期号"); err != nil {
		t.Fatal(err)
	}
	qoderInvalidateCredsCache()
	list := qoderCredsStatus(true)
	if len(list) != 1 {
		t.Fatalf("应有 1 份，得到 %+v", list)
	}
	if list[0].OK || list[0].State != "登录失效" {
		t.Fatalf("上游 401 应标成登录失效：%+v", list[0])
	}
	if list[0].Reason == "" {
		t.Fatal("失效必须给原因")
	}
	// 关键：Until 不能塞东西，否则前端会渲染成「限流中」
	if list[0].Until != "" {
		t.Fatalf("Until 是限流专用字段，必须留空，得到 %q", list[0].Until)
	}
}

func TestQoderCredsStatusMarksExpiredSnapshot(t *testing.T) {
	appData := fakeQoderEnv(t)
	var hit bool
	fakeQoderUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Write([]byte(`{"userId":"u","userQuota":{"remaining":1,"total":2}}`))
	})
	// 手写一份 meta.json 里已过期的快照（auth 内容与 live 不同即可）
	dir := filepath.Join(qoderVaultRoot(), "老号")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.v1.dat"),
		[]byte(`{"token":"tok-old","user":{"id":"uid-old","name":"老号"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"),
		[]byte(`{"name":"老号","nick":"老号","uid":"uid-old","expiresAt":"`+past+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFakeQoderCred(t, appData, "uid-live", "在用号", "tok-live")

	qoderInvalidateCredsCache()
	list := qoderCredsStatus(true)
	var found bool
	for _, c := range list {
		if c.Name == "老号" {
			found = true
			if c.OK || c.State != "已过期" {
				t.Fatalf("过期快照应标已过期：%+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("列表里没有老号：%+v", list)
	}
	if hit {
		t.Fatal("已过期的快照不该再打上游")
	}
}

func TestQoderCredsStatusUsesCache(t *testing.T) {
	appData := fakeQoderEnv(t)
	var hits int
	fakeQoderUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"userId":"u","userQuota":{"remaining":5,"total":10}}`))
	})
	writeFakeQoderCred(t, appData, "uid-a", "小号A", "tok-a")
	if _, err := qoderVaultSave("小号A"); err != nil {
		t.Fatal(err)
	}
	qoderInvalidateCredsCache()
	qoderCredsStatus(false)
	qoderCredsStatus(false)
	qoderCredsStatus(false)
	if hits != 1 {
		t.Fatalf("TTL 内应只查一次上游，实际 %d 次", hits)
	}
	qoderCredsStatus(true)
	if hits != 2 {
		t.Fatalf("force=1 应重查，实际 %d 次", hits)
	}
}

func TestQoderCredCurrentNameMatchesLive(t *testing.T) {
	appData := fakeQoderEnv(t)
	fakeQoderUsageServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"userId":"u","userQuota":{"remaining":5,"total":10}}`))
	})
	writeFakeQoderCred(t, appData, "uid-a", "小号A", "tok-a")
	if _, err := qoderVaultSave("小号A"); err != nil {
		t.Fatal(err)
	}
	if got := qoderCredCurrentName(); got != "小号A" {
		t.Fatalf("当前凭证名应是小号A，得到 %q", got)
	}
	// 换号后（live 文件变了但没入库）就匹配不上
	writeFakeQoderCred(t, appData, "uid-z", "陌生号", "tok-z")
	if got := qoderCredCurrentName(); got != "" {
		t.Fatalf("未入库的凭据不该匹配到任何快照，得到 %q", got)
	}
}
