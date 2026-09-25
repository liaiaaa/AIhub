package main

// 凭证发现逻辑的本地测试（go test）：
//   - credDirs 必须包含<凭证目录C>
//   - <凭证目录C>目录里没有标准名文件，dirHasCredential 仍要认出 ai-<uuid>.info
//   - AuthFilePath 的 glob 回退要能解析出真实文件
//
// 注意：依赖本机真实凭证目录。测试自身通过 repoRoot() 定位仓库根，
// 不再硬编码绝对路径（曾因写死 D:\C\Desktop\workbuddy-hub 而在目录改名后失败）。

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot 返回仓库根（hub-go 的上一级），由测试文件自身位置推导，
// 因此与仓库放到哪个盘、哪一层目录无关。
func repoRoot(t *testing.T) string {
	t.Helper()
	// 本测试文件位于 <repo>/hub-go/ 下；go test 以源码目录为工作目录。
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("无法获取工作目录: %v", err)
	}
	root := filepath.Dir(wd)
	if _, err := os.Stat(filepath.Join(root, "hub-go", "go.mod")); err != nil {
		t.Skipf("仓库根布局不符合预期（%s 下没有 hub-go/go.mod），跳过依赖真实凭证的测试", root)
	}
	return root
}

func TestCredDiscovery(t *testing.T) {
	// go test 的二进制在临时目录，hubRoot（exe 上级的上级）会算错，手动指回真实根。
	hubRoot = repoRoot(t)
	dirs := credDirs()
	if _, ok := dirs["<凭证目录C>"]; !ok {
		t.Fatalf("credDirs 缺少<凭证目录C>: %v", dirs)
	}
	tian := dirs["<凭证目录C>"]
	if !dirHasCredential(tian) {
		t.Skipf("<凭证目录C>目录不存在或无凭证文件（%s），跳过", tian)
	}
	if !dirHasCredential(filepath.Join(hubRoot, "<凭证目录A>")) {
		t.Fatalf("<凭证目录A>目录未被识别")
	}
	p := NewIntlProxy(tian)
	got := p.AuthFilePath()
	if got == "" || !fileExists(got) {
		t.Fatalf("AuthFilePath 未解析出真实文件: %q", got)
	}
	t.Logf("<凭证目录C>解析到: %s", got)
}
