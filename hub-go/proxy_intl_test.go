package main

// 凭证发现逻辑的本地测试（go test）：
//   - credDirs 必须包含<凭证目录C>
//   - <凭证目录C>目录里没有标准名文件，dirHasCredential 仍要认出 ai-<uuid>.info
//   - AuthFilePath 的 glob 回退要能解析出真实文件
//
// 注意：依赖本机真实凭证目录（测试里把 hubRoot 指回真实根）。

import (
	"path/filepath"
	"testing"
)

func TestCredDiscovery(t *testing.T) {
	// go test 的二进制在临时目录，hubRoot（exe 上级的上级）会算错，手动指回真实根。
	hubRoot = `D:\C\Desktop\workbuddy-hub`
	dirs := credDirs()
	if _, ok := dirs["<凭证目录C>"]; !ok {
		t.Fatalf("credDirs 缺少<凭证目录C>: %v", dirs)
	}
	tian := dirs["<凭证目录C>"]
	if !dirHasCredential(tian) {
		t.Fatalf("<凭证目录C>目录未被识别为有效凭证目录: %s", tian)
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
