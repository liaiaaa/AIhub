package main

import (
	"os"
	"path/filepath"
	"testing"
)

// credDirs 从硬编码改为「内置三个 + 数据根自动扫描」（2026-10-12）。
// 核心风险：不能误捡国内线路的凭证目录（国内用 workbuddy-desktop.info，
// 国际用 workbuddy-desktop-ai.info，两套后端两套 token）。
func TestCredDirsScan(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()

	mk := func(dir, file string) {
		if err := os.MkdirAll(filepath.Join(hubRoot, dir), 0755); err != nil {
			t.Fatal(err)
		}
		if file == "" {
			return // 只建空目录
		}
		if err := os.WriteFile(filepath.Join(hubRoot, dir, file), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// 国际线路的凭证：标准名 + ai-<uuid>.info 两种
	mk("<凭证目录D>", "workbuddy-desktop-ai.info")
	mk("张三凭证", "ai-9685e9b1-187c-4c05-960b-8e1e35a17a67.info")
	// 国内线路的凭证：文件名不同，绝不能被国际线路认领
	mk("国内workbuddy凭证", "workbuddy-desktop.info")
	// 干扰项：空目录 / 只有无关文件 / 隐藏目录
	mk("空目录", "")
	mk("无关目录", "readme.txt")
	mk(".hidden凭证", "workbuddy-desktop-ai.info")

	dirs := credDirs()

	// 内置三个必须始终在（哪怕目录为空）
	for _, k := range []string{"<凭证目录A>", "<凭证目录B>", "<凭证目录C>"} {
		if _, ok := dirs[k]; !ok {
			t.Fatalf("内置凭证 %s 应始终存在：%v", k, dirs)
		}
	}
	// 自动扫描到的
	if _, ok := dirs["<凭证目录D>"]; !ok {
		t.Fatalf("<凭证目录D> 未被扫描到：%v", dirs)
	}
	if _, ok := dirs["张三凭证"]; !ok {
		t.Fatalf("ai-<uuid>.info 形态的目录未被扫描到：%v", dirs)
	}
	// 关键：国内凭证目录不能被认领
	if _, ok := dirs["国内workbuddy凭证"]; ok {
		t.Fatalf("❌ 国内线路的凭证目录被国际线路误捡了：%v", dirs)
	}
	// 干扰项
	if _, ok := dirs["空目录"]; ok {
		t.Fatalf("空目录不该入选：%v", dirs)
	}
	if _, ok := dirs["无关目录"]; ok {
		t.Fatalf("无凭证文件的目录不该入选：%v", dirs)
	}
	if _, ok := dirs[".hidden凭证"]; ok {
		t.Fatalf("隐藏目录不该入选：%v", dirs)
	}
}

// 路径必须是绝对路径（拼在 hubRoot 下），不是相对名
func TestCredDirsScanAbsolutePath(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	if err := os.MkdirAll(filepath.Join(hubRoot, "<凭证目录D>"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubRoot, "<凭证目录D>", "workbuddy-desktop-ai.info"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := credDirs()["<凭证目录D>"]
	if want := filepath.Join(hubRoot, "<凭证目录D>"); got != want {
		t.Fatalf("路径应为 %s，实际 %s", want, got)
	}
}

// 扫描结果必须能被 dirHasCredential 认可（控制台据此判断「可用」）
func TestScannedDirIsUsable(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	dir := filepath.Join(hubRoot, "<凭证目录D>")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop-ai.info"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !dirHasCredential(credDirs()["<凭证目录D>"]) {
		t.Fatalf("扫描到的目录应被 dirHasCredential 认可")
	}
}
