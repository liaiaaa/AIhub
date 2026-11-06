package main

// 同账号重复快照的治理（2026-11-06 用户报「实际就 2 个凭证，为什么显示四个」）。
//
// 成因：判重只看**字节是否相同**，而 token 每次刷新字节都变 → 同一个账号被存成
// 多条快照（实测仓库里 nick5461534038 有 3 份：本体 + -2 + auto-20261106-172045，
// uid 完全相同），UI 把它们当 4 个「凭证」平铺出来。
//
// 修法三层：入库按 uid 覆盖、展示按 uid 去重、启动清理历史重复项。
// 注：itoa / contains 已由包内其他文件提供，这里不重复定义。

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// seedVaultEntry 直接往快照仓库塞一份历史条目（绕过入库逻辑，模拟旧版本留下的脏数据）。
func seedVaultEntry(t *testing.T, name, uid, nick, token, savedAt string) {
	t.Helper()
	dir := filepath.Join(qoderVaultRoot(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schemaVersion":1,"token":"` + token + `","refreshToken":"r",` +
		`"expiresAt":"2026-12-05T09:38:57Z","user":{"id":"` + uid + `","name":"` + nick + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.v1.dat"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mb := []byte(`{"name":"` + name + `","nick":"` + nick + `","uid":"` + uid +
		`","email":"","expiresAt":"2026-12-05T09:38:57Z","savedAt":"` + savedAt +
		`","size":` + strconv.Itoa(len(body)) + `}`)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readQoderLive(t *testing.T, appData string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(appData, "auth.v1.dat"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 核心回归：同一账号刷新 token 后重新入库，必须覆盖原条目，不新增。
func TestQoderVaultRefreshSameUIDDoesNotDuplicate(t *testing.T) {
	appData := fakeQoderEnv(t)
	writeFakeQoderCred(t, appData, "uid-b", "nickB", "tok-old")
	first, err := ensureCurrentSnapshotted(readQoderLive(t, appData))
	if err != nil {
		t.Fatal(err)
	}

	// token 刷新：字节变了，但 uid 没变
	writeFakeQoderCred(t, appData, "uid-b", "nickB", "tok-new")
	second, err := ensureCurrentSnapshotted(readQoderLive(t, appData))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("同账号刷新应复用同一快照名，实际 %q → %q", first, second)
	}

	all, err := qoderVaultList()
	if err != nil {
		t.Fatal(err)
	}
	same := 0
	for _, a := range all {
		if a.UID == "uid-b" {
			same++
		}
	}
	if same != 1 {
		t.Fatalf("同账号刷新后应只有 1 条快照，实际 %d 条：%+v", same, all)
	}
	b, err := os.ReadFile(qoderVaultPath(first))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(b), "tok-new") {
		t.Fatalf("快照没更新成新 token：%s", string(b)[:min(90, len(string(b)))])
	}
}

// 展示层：同 uid 只出一条（取最新）。
func TestQoderVaultDedupeByUID(t *testing.T) {
	in := []qoderAccountMeta{
		{Name: "auto-20261106-172045", UID: "uid-b", SavedAt: "2026-11-06 17:20:45"},
		{Name: "nickB-2", UID: "uid-b", SavedAt: "2026-11-06 12:59:34"},
		{Name: "nickB", UID: "uid-b", SavedAt: "2026-11-05 18:24:29"},
		{Name: "aliyunA", UID: "uid-a", SavedAt: "2026-11-05 18:30:09"},
		{Name: "无名历史项", UID: "", SavedAt: "2026-11-01 10:00:00"},
	}
	out := qoderVaultDedupeByUID(in)
	if len(out) != 3 {
		t.Fatalf("2 个账号 + 1 条无 uid，应剩 3 条，实际 %d：%+v", len(out), out)
	}
	if out[0].Name != "auto-20261106-172045" {
		t.Errorf("uid-b 应保留最新那条，实际 %q", out[0].Name)
	}
	if out[1].Name != "aliyunA" {
		t.Errorf("uid-a 那条应保留，实际 %q", out[1].Name)
	}
	if out[2].UID != "" {
		t.Errorf("uid 为空的条目不该被去掉")
	}
}

// 启动清理：同账号只留最新一份，并把 auto-* 归一成可读昵称。
func TestQoderVaultPruneDupes(t *testing.T) {
	fakeQoderEnv(t)
	seedVaultEntry(t, "nickB", "uid-b", "nickB", "tok-1", "2026-11-05 18:24:29")
	seedVaultEntry(t, "nickB-2", "uid-b", "nickB", "tok-2", "2026-11-06 12:59:34")
	seedVaultEntry(t, "auto-20261106-172045", "uid-b", "nickB", "tok-3", "2026-11-06 17:20:45")
	seedVaultEntry(t, "aliyunA", "uid-a", "aliyunA", "tok-a", "2026-11-05 18:30:09")

	n := qoderVaultPruneDupes()
	// 3 次动作：删 nickB-2、删被取代的旧 nickB、把最新的 auto-* 改名为 nickB
	if n != 3 {
		t.Fatalf("应做 3 件事（删 2 条旧副本 + 1 次改名），实际 %d", n)
	}
	all, err := qoderVaultList()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("清理后每个账号一条，应剩 2 条，实际 %d：%+v", len(all), all)
	}
	byName := map[string]qoderAccountMeta{}
	for _, a := range all {
		byName[a.Name] = a
	}
	// nickB 这个名字现在装的必须是**最新**那份内容（tok-3），不是旧的 tok-1
	kept, ok := byName["nickB"]
	if !ok {
		names := []string{}
		for _, a := range all {
			names = append(names, a.Name)
		}
		t.Fatalf("留下的那份应改名为可读昵称 nickB，实际列表 %v", names)
	}
	b, err := os.ReadFile(qoderVaultPath(kept.Name))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(b), "tok-3") {
		t.Errorf("nickB 里应是最新的 tok-3，实际 %s", string(b)[:min(90, len(string(b)))])
	}
	if _, err := os.Stat(filepath.Join(qoderVaultRoot(), "nickB-2")); err == nil {
		t.Errorf("nickB-2 应已被清理")
	}
	if _, err := os.Stat(filepath.Join(qoderVaultRoot(), "auto-20261106-172045")); err == nil {
		t.Errorf("auto-* 应已改名，不该还留占位名")
	}
	if _, ok := byName["aliyunA"]; !ok {
		t.Errorf("另一个账号不该被动到")
	}
}
