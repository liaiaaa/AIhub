package main

// Qoder 每日签到的多账号回归测试（2026-11-08）。
//
// 用户报：两个 Qoder 账号里有一个签到失败、另一个得手动签。
// 根因：qoderCheckin() 只取 qoderDeviceToken()（当前桌面端
// live 那一个账号），而 qoder-accounts/ 里存着 2 份快照；切号之前点了签到，
// 之后切到另一个号，全局 checkin.ok=true 就把「这个号没签」盖住了。
//
// 这里用明文假凭据 + httptest 假上游，不打真网络、不碰真实凭据。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 让 qoderMachineInfo 立刻返回空，避免测试里真跑桌面端 runtime-info（最多 8s）。
func stubQoderMachineInfo(t *testing.T) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "runtime-info")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '{}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QODER_RUNTIME_INFO_EXE", exe)
}

// fakeQoderCheckinServer 起一个按 token 区分账号的假 campaigns 服务：
// claimable 里的 token 返回 1 个 CLAIMABLE 活动，其余返回 CLAIMED。
// fail 里的 token 直接 500，用来验证「哪个账号失败」要出现在结果里。
func fakeQoderCheckinServer(t *testing.T, claimable, fail map[string]bool) (*httptest.Server, *sync.Mutex, map[string]int, *int) {
	t.Helper()
	stubQoderMachineInfo(t)
	var mu sync.Mutex
	seen := map[string]int{}
	claims := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[tok]++
		mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim") {
			mu.Lock()
			claims++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"credit":100}`))
			return
		}
		if fail[tok] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"upstream down"}`))
			return
		}
		status := "CLAIMED"
		if claimable[tok] {
			status = "CLAIMABLE"
		}
		_, _ = w.Write([]byte(`{"code":0,"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"` + status + `"}]}`))
	}))
	t.Cleanup(srv.Close)
	old := qoderCheckinBase
	qoderCheckinBase = srv.URL
	t.Cleanup(func() { qoderCheckinBase = old })
	return srv, &mu, seen, &claims
}

func seedQoderVault(t *testing.T, appData, name, uid, token string) {
	t.Helper()
	writeFakeQoderCred(t, appData, uid, name, token)
	if _, err := qoderVaultSave(name); err != nil {
		t.Fatalf("存快照 %s 失败：%v", name, err)
	}
}

// TestQoderCheckinCoversEveryVaultAccount：一次签到必须覆盖快照仓库里每个账号，
// 而不是只签当前 live 那一个（复现用户报的漏签）。
func TestQoderCheckinCoversEveryVaultAccount(t *testing.T) {
	appData := fakeQoderEnv(t)
	seedQoderVault(t, appData, "账号A", "uid-a", "tok-a")
	seedQoderVault(t, appData, "账号B", "uid-b", "tok-b")

	// live 停在 A：模拟「点签到时是 A，之后才切到 B」的真实时间线。
	writeFakeQoderCred(t, appData, "uid-a", "账号A", "tok-a")
	liveBefore, err := os.ReadFile(filepath.Join(appData, "auth.v1.dat"))
	if err != nil {
		t.Fatal(err)
	}

	_, mu, seen, claims := fakeQoderCheckinServer(t,
		map[string]bool{"tok-b": true}, nil)

	res := qoderCheckin()

	if !res.OK {
		t.Fatalf("两个账号都应成功：%s", res.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["tok-a"] == 0 || seen["tok-b"] == 0 {
		t.Fatalf("两个账号都必须被请求，实际 %v", seen)
	}
	if *claims != 1 {
		t.Fatalf("只有 B 有可领活动，claim 应恰好 1 次，实际 %d", *claims)
	}
	for _, n := range []string{"账号A", "账号B"} {
		if !strings.Contains(res.Detail, n) {
			t.Fatalf("结果文案应逐个列出账号 %s：%s", n, res.Detail)
		}
	}
	liveAfter, _ := os.ReadFile(filepath.Join(appData, "auth.v1.dat"))
	if string(liveBefore) != string(liveAfter) {
		t.Fatal("签到不该改写桌面端 live 凭据（不许切号）")
	}
	t.Logf("文案：%s", res.Detail)
}

// TestQoderCheckinIncludesUnvaultedLiveAccount：当前桌面端账号还没来得及进
// 快照仓库时，也不能漏签（否则「刚换的号」又会重现用户这次的问题）。
func TestQoderCheckinIncludesUnvaultedLiveAccount(t *testing.T) {
	appData := fakeQoderEnv(t)
	seedQoderVault(t, appData, "账号A", "uid-a", "tok-a")
	// live 是一个快照仓库里没有的账号
	writeFakeQoderCred(t, appData, "uid-live", "新登录账号", "tok-live")

	_, mu, seen, claims := fakeQoderCheckinServer(t,
		map[string]bool{"tok-live": true}, nil)

	res := qoderCheckin()
	if !res.OK {
		t.Fatalf("两个账号都应成功：%s", res.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["tok-a"] == 0 || seen["tok-live"] == 0 {
		t.Fatalf("快照账号和未入库的 live 账号都必须被签到，实际 %v", seen)
	}
	if *claims != 1 {
		t.Fatalf("只有 live 有可领活动，claim 应恰好 1 次，实际 %d", *claims)
	}
}

// TestQoderCheckinUsesFreshLiveTokenForSameAccount：同一账号的快照 token 过期后，
// 桌面端 live 凭据是新的；不能拿旧快照再打一次（会多一个无效账号、漏签真账号）。
func TestQoderCheckinUsesFreshLiveTokenForSameAccount(t *testing.T) {
	appData := fakeQoderEnv(t)
	seedQoderVault(t, appData, "账号A", "uid-a", "tok-a-old")
	// 同一个 uid，桌面端刷新后的 token
	writeFakeQoderCred(t, appData, "uid-a", "账号A", "tok-a-new")

	_, mu, seen, claims := fakeQoderCheckinServer(t,
		map[string]bool{"tok-a-new": true}, nil)

	res := qoderCheckin()
	if !res.OK {
		t.Fatalf("刷新后的账号应签到成功：%s", res.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["tok-a-new"] == 0 || seen["tok-a-old"] != 0 {
		t.Fatalf("应只用新的 live token，不要用旧快照 token，实际 %v", seen)
	}
	if *claims != 1 {
		t.Fatalf("同一账号只应 claim 1 次，实际 %d", *claims)
	}
}

// TestQoderCheckinVaultReadFailureStillSignsLive：快照仓库本身读不了时，
// 不能把「当前桌面端账号」也一起放弃（宁可只签 live，也不能一个都不签）。
func TestQoderCheckinVaultReadFailureStillSignsLive(t *testing.T) {
	appData := fakeQoderEnv(t)
	writeFakeQoderCred(t, appData, "uid-live", "桌面端账号", "tok-live")
	if err := os.MkdirAll(filepath.Dir(qoderVaultRoot()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qoderVaultRoot(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, mu, seen, claims := fakeQoderCheckinServer(t,
		map[string]bool{"tok-live": true}, nil)
	res := qoderCheckin()
	if !res.OK {
		t.Fatalf("快照仓库不可读时至少要把 live 账号签上：%s", res.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["tok-live"] == 0 || *claims != 1 {
		t.Fatalf("live 账号应被签到且领取 1 次，实际 seen=%v claims=%d", seen, *claims)
	}
}

// TestQoderCheckinReportsFailingAccount：某个账号签到失败时，整体 OK=false
// 且文案要点名是哪个账号 —— 否则又会变成「全局已签」把漏签盖住。
func TestQoderCheckinReportsFailingAccount(t *testing.T) {
	appData := fakeQoderEnv(t)
	seedQoderVault(t, appData, "账号A", "uid-a", "tok-a")
	seedQoderVault(t, appData, "账号B", "uid-b", "tok-b")

	_, _, _, _ = fakeQoderCheckinServer(t,
		map[string]bool{"tok-b": true},
		map[string]bool{"tok-a": true})

	res := qoderCheckin()
	if res.OK {
		t.Fatalf("有一个账号失败，整体应为 false：%s", res.Detail)
	}
	if !strings.Contains(res.Detail, "账号A") {
		t.Fatalf("失败文案要点名账号：%s", res.Detail)
	}
	if !strings.Contains(res.Detail, "账号B") {
		t.Fatalf("成功账号也要列出：%s", res.Detail)
	}
}

// TestQoderCheckinFallsBackToLiveWithoutVault：没有快照仓库时保持旧行为，
// 仍然签当前桌面端账号（不能因为改成多账号就把单机用户弄成「没有可用凭据」）。
func TestQoderCheckinFallsBackToLiveWithoutVault(t *testing.T) {
	appData := fakeQoderEnv(t)
	writeFakeQoderCred(t, appData, "uid-live", "桌面端账号", "tok-live")

	_, mu, seen, claims := fakeQoderCheckinServer(t,
		map[string]bool{"tok-live": true}, nil)

	res := qoderCheckin()
	if !res.OK {
		t.Fatalf("无快照时应回退签 live 账号：%s", res.Detail)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["tok-live"] == 0 {
		t.Fatalf("live 账号应被请求，实际 %v", seen)
	}
	if *claims != 1 {
		t.Fatalf("应领取 1 次，实际 %d", *claims)
	}
}
