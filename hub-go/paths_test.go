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

// TestLogsAloneDoNotMakeDataRoot 回归测试：
// 只有日志（没有 state.json / 凭证目录）的目录**不能**被判定为数据根。
//
// 曾经的 bug：标志里包含了 converter-*.log，于是一旦日志被误写到某目录，
// 下次解析就会认为该目录是数据根，错误自我固化、永远回不到真正的根。
// 实际表现：exe 在 bin\ 时日志被写进 hub-go\ 而不是仓库根。
func TestLogsAloneDoNotMakeDataRoot(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"hub.log", "converter-cn.log", "converter-intl.log"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if looksLikeDataRoot(dir) {
		t.Fatalf("只有日志的目录不应被判定为数据根（会导致日志写错位置且自我固化）: %s", dir)
	}
}

// TestResolveHubRootFindsRootAboveBin 回归测试：
// exe 放在 <root>\hub-go\bin\ 时，也要能向上找到 <root> 作为数据根。
// 这是桌面快捷方式 / build.bat 的默认布局，必须工作。
func TestResolveHubRootFindsRootAboveBin(t *testing.T) {
	// 构造 <tmp>\hub-go\bin\ 布局，并在 <tmp> 放数据根标志
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "hub-go", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "state.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// 直接验证候选遍历逻辑：从 binDir 向上必须能命中 tmp
	dir := binDir
	found := ""
	for i := 0; i < 5; i++ {
		if looksLikeDataRoot(dir) {
			found = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if found == "" {
		t.Fatalf("从 %s 向上未能找到数据根（期望 %s）", binDir, tmp)
	}
	// 注意：t.TempDir 可能返回带符号链接的短路径，用 EvalSymlinks 归一后比较
	want, _ := filepath.EvalSymlinks(tmp)
	got, _ := filepath.EvalSymlinks(found)
	if got != want {
		t.Fatalf("解析到错误的数据根: got=%q want=%q", got, want)
	}
}

// TestResolveHubRootHonoursEnv 确认 AIHUB_ROOT 能显式覆盖，
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
