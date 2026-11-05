package main

// Qoder 账号快照的测试（2026-11-05）。
//
// 用**明文 JSON** 当假凭据：qoderOpenCreds 对非 vNN 信封的输入会按明文处理
// （见 proxy_qoder_oskey_other.go:52），所以不需要真的 keyring 就能覆盖
// 保存 / 列表 / 切换 / 删除的全部逻辑。

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeQoderEnv(t *testing.T) (appData string) {
	t.Helper()
	root := t.TempDir()
	appData = filepath.Join(root, "qoder-app")
	if err := os.MkdirAll(appData, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QODER_APP_DATA", appData)
	old := hubRoot
	hubRoot = filepath.Join(root, "data")
	t.Cleanup(func() { hubRoot = old })
	return appData
}

func writeFakeQoderCred(t *testing.T, appData, uid, nick, token string) {
	t.Helper()
	body := `{"schemaVersion":1,"token":"` + token + `","refreshToken":"r",` +
		`"expiresAt":"2026-12-05T09:38:57Z","user":{"id":"` + uid + `","name":"` + nick + `"}}`
	if err := os.WriteFile(filepath.Join(appData, "auth.v1.dat"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQoderVaultSaveAndList(t *testing.T) {
	appData := fakeQoderEnv(t)
	writeFakeQoderCred(t, appData, "uid-a", "小号A", "tok-a")

	meta, err := qoderVaultSave("A")
	if err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if meta.Nick != "小号A" || meta.UID != "uid-a" {
		t.Fatalf("元信息不对：%+v", meta)
	}

	list, err := qoderVaultList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "A" || !list[0].Current {
		t.Fatalf("列表不对：%+v", list)
	}
}

func TestQoderVaultUseSwitchesAndBacksUp(t *testing.T) {
	appData := fakeQoderEnv(t)

	// 先存 A
	writeFakeQoderCred(t, appData, "uid-a", "小号A", "tok-a")
	if _, err := qoderVaultSave("A"); err != nil {
		t.Fatal(err)
	}
	// 再换成 B（模拟桌面端换号），存 B
	writeFakeQoderCred(t, appData, "uid-b", "小号B", "tok-b")
	if _, err := qoderVaultSave("B"); err != nil {
		t.Fatal(err)
	}

	// 切回 A：当前 B 必须被自动备份，不能丢
	msg, err := qoderVaultUse("A")
	if err != nil {
		t.Fatalf("切换失败：%v", err)
	}
	if msg == "" {
		t.Fatal("切换应返回提示文案")
	}
	cur, err := qoderVaultCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if cur.UID != "uid-a" {
		t.Fatalf("切换后应是 A，得到 %s", cur.UID)
	}
	list, _ := qoderVaultList()
	var hasAuto bool
	for _, a := range list {
		if len(a.Name) > 5 && a.Name[:5] == "auto-" && a.UID == "uid-b" {
			hasAuto = true
		}
	}
	if !hasAuto {
		t.Fatalf("切换前的 B 应被自动备份，列表：%+v", list)
	}
}

func TestQoderVaultDeleteAndMissing(t *testing.T) {
	appData := fakeQoderEnv(t)
	writeFakeQoderCred(t, appData, "uid-a", "小号A", "tok-a")
	if _, err := qoderVaultSave("A"); err != nil {
		t.Fatal(err)
	}
	if err := qoderVaultDelete("A"); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if list, _ := qoderVaultList(); len(list) != 0 {
		t.Fatalf("删除后应为空：%+v", list)
	}
	if err := qoderVaultDelete("A"); err == nil {
		t.Fatal("删除不存在的快照应报错")
	}
	if _, err := qoderVaultUse("nope"); err == nil {
		t.Fatal("切换不存在的快照应报错")
	}
}

func TestQoderVaultRejectsUnsafeNames(t *testing.T) {
	fakeQoderEnv(t)
	for _, bad := range []string{"", "../escape", "a/b", "a b", "x;rm"} {
		if _, err := sanitizeVaultName(bad); err == nil {
			t.Errorf("名字 %q 应被拒绝", bad)
		}
	}
	for _, ok := range []string{"小号", "acc-1", "a_b.c"} {
		if _, err := sanitizeVaultName(ok); err != nil {
			t.Errorf("名字 %q 应被接受：%v", ok, err)
		}
	}
}

func TestQoderVaultSaveRejectsUndecryptable(t *testing.T) {
	appData := fakeQoderEnv(t)
	// v01 信封 + 垃圾内容 → 解不开，不能存
	if err := os.WriteFile(filepath.Join(appData, "auth.v1.dat"), []byte("v01\x00\x01\x02garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := qoderVaultSave("bad"); err == nil {
		t.Fatal("解不开的凭据不该被保存")
	}
}
