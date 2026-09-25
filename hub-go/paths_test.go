package main

// 数据根目录（hubRoot）解析规则的测试。
//
// 背景：hubRoot 曾硬编码为 exe 的上级的上级，导致「数据放在哪」与「exe 放在哪」耦合。
// 这里验证新的解析逻辑：显式覆盖优先、能识别真正的数据根、exe 移动后不会写错目录。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDataPathUsesHubRoot 确认 dataPath 始终相对于 hubRoot。
func TestDataPathUsesHubRoot(t *testing.T) {
	old := hubRoot
	defer func() { hubRoot = old }()

	hubRoot = filepath.Join("X:", "some", "root")
	got := dataPath("hub.log")
	want := filepath.Join("X:", "some", "root", "hub.log")
	if got != want {
		t.Fatalf("dataPath 结果不符：got=%q want=%q", got, want)
	}
}

// TestLooksLikeDataRoot 用真实的标志性文件判断目录是否像数据根。
func TestLooksLikeDataRoot(t *testing.T) {
	empty := t.TempDir()
	if looksLikeDataRoot(empty) {
		t.Fatalf("空目录不应被判定为数据根: %s", empty)
	}

	// 放一个 state.json 就应被识别
	withState := t.TempDir()
	if err := os.WriteFile(filepath.Join(withState, "state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if !looksLikeDataRoot(withState) {
		t.Fatalf("含 state.json 的目录应被判定为数据根: %s", withState)
	}

	// 只有凭证目录也应被识别
	withCred := t.TempDir()
	if err := os.MkdirAll(filepath.Join(withCred, "<凭证目录A>"), 0755); err != nil {
		t.Fatal(err)
	}
	if !looksLikeDataRoot(withCred) {
		t.Fatalf("含凭证目录的目录应被判定为数据根: %s", withCred)
	}
}

// TestResolveHubRootHonoursEnv 确认 WORKBUDDY_HUB_ROOT 能显式覆盖，
// 这是「exe 与数据分离部署」的正式开关。
func TestResolveHubRootHonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(dataRootEnv, dir)

	got := resolveHubRoot()
	if got != dir {
		t.Fatalf("环境变量覆盖未生效：got=%q want=%q", got, dir)
	}
}

// TestResolveHubRootNeverReturnsEmpty 保证任何情况下都不会返回空串
// （空 hubRoot 会让日志写到当前目录，属于隐性故障）。
func TestResolveHubRootNeverReturnsEmpty(t *testing.T) {
	t.Setenv(dataRootEnv, "")
	if got := resolveHubRoot(); got == "" {
		t.Fatal("resolveHubRoot 返回了空字符串")
	}
}
