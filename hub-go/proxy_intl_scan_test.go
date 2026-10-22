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
	mk("cred-d", "workbuddy-desktop-ai.info")
	mk("张三凭证", "ai-00000000-0000-4000-8000-000000000000.info")
	// 国内线路的凭证：文件名不同，绝不能被国际线路认领
	mk("国内workbuddy凭证", "workbuddy-desktop.info")
	// 干扰项：空目录 / 只有无关文件 / 隐藏目录
	mk("空目录", "")
	mk("无关目录", "readme.txt")
	mk(".hidden凭证", "workbuddy-desktop-ai.info")

	dirs := credDirs()

	// 2026-10-21 开源审查后不再有「内置凭证名」：生产代码里的三个个人称呼
	// （<凭证目录A>/<凭证目录B>/<凭证目录C>）已删除，改为完全靠扫描发现 —— 开源后公开个人
	// 称呼没有意义，陌生人的目录名也完全不同。这里改为断言「不写死任何名字
	// 也能把有凭据的目录全部扫出来」。
	if len(dirs) == 0 {
		t.Fatalf("一个凭证目录都没扫到：%v", dirs)
	}
	// 自动扫描到的
	if _, ok := dirs["cred-d"]; !ok {
		t.Fatalf("cred-d 未被扫描到：%v", dirs)
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
	if err := os.MkdirAll(filepath.Join(hubRoot, "cred-d"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hubRoot, "cred-d", "workbuddy-desktop-ai.info"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	got := credDirs()["cred-d"]
	if want := filepath.Join(hubRoot, "cred-d"); got != want {
		t.Fatalf("路径应为 %s，实际 %s", want, got)
	}
}

// 扫描结果必须能被 dirHasCredential 认可（控制台据此判断「可用」）
func TestScannedDirIsUsable(t *testing.T) {
	old := hubRoot
	hubRoot = t.TempDir()
	defer func() { hubRoot = old }()
	dir := filepath.Join(hubRoot, "cred-d")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-desktop-ai.info"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !dirHasCredential(credDirs()["cred-d"]) {
		t.Fatalf("扫描到的目录应被 dirHasCredential 认可")
	}
}
