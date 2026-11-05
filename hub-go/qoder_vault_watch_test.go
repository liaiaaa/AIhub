package main

// Qoder 凭据变更监听的测试（2026-11-05）。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQoderVaultWatchArchivesOverwritten(t *testing.T) {
	appData := fakeQoderEnv(t)
	livePath := filepath.Join(appData, "auth.v1.dat")

	// 旧号
	writeFakeQoderCred(t, appData, "uid-old", "老号", "tok-old")
	if _, err := qoderVaultWatchTick(); err != nil {
		t.Fatalf("首轮应只记基线：%v", err)
	}
	// 首轮：当前账号应被收进仓库（否则列表里看不到正在用的号），
	// 但不应产生 auto-* 的「旧号」留档。
	list0, _ := qoderVaultList()
	if len(list0) != 1 || list0[0].UID != "uid-old" {
		t.Fatalf("首轮应把当前账号入库一份，得到 %+v", list0)
	}
	for _, a := range list0 {
		if len(a.Name) > 5 && a.Name[:5] == "auto-" {
			t.Fatalf("首轮不该产生旧号留档：%+v", list0)
		}
	}

	// 模拟桌面端换号：直接覆盖
	writeFakeQoderCred(t, appData, "uid-new", "新号", "tok-new")
	name, err := qoderVaultWatchTick()
	if err != nil {
		t.Fatalf("换号后应自动留档：%v", err)
	}
	if name == "" {
		t.Fatal("换号后应返回自动快照名")
	}

	// 旧号必须被存下来
	list, _ := qoderVaultList()
	var found bool
	for _, a := range list {
		if a.UID == "uid-old" && a.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("旧号没被留档：%+v", list)
	}
	// 基线要更新成新内容，避免下一轮重复留档
	seen, _ := os.ReadFile(qoderVaultLastSeenPath())
	live, _ := os.ReadFile(livePath)
	if string(seen) != string(live) {
		t.Fatal("基线没更新")
	}
	// 再跑一轮：没变化就不该再产生快照
	before := len(list)
	if n2, _ := qoderVaultWatchTick(); n2 != "" {
		t.Fatal("内容没变时不该留档")
	}
	if after, _ := qoderVaultList(); len(after) != before {
		t.Fatalf("快照数变了：%d → %d", before, len(after))
	}
}

func TestQoderVaultWatchIgnoresMissingFile(t *testing.T) {
	fakeQoderEnv(t) // 不写 auth.v1.dat
	if name, err := qoderVaultWatchTick(); err != nil || name != "" {
		t.Fatalf("没有凭据文件时应静默跳过，得到 name=%q err=%v", name, err)
	}
}

func TestQoderVaultStoreRejectsGarbage(t *testing.T) {
	fakeQoderEnv(t)
	if _, err := qoderVaultStore("bad", []byte("v01\x00garbage")); err == nil {
		t.Fatal("解不开的字节不该被存成快照")
	}
}
