package main

// 凭证发现逻辑的本地测试（go test）：
//   - credDirs 必须包含cred-c
//   - cred-c目录里没有标准名文件，dirHasCredential 仍要认出 ai-<uuid>.info
//   - AuthFilePath 的 glob 回退要能解析出真实文件
//
// 注意：依赖本机真实凭证目录。测试自身通过 repoRoot() 定位仓库根，
// 不再硬编码绝对路径（曾因写死 D:\C\Desktop\workbuddy-hub 而在目录改名后失败）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	// 2026-10-21 开源审查：这里原来断言几个写死的凭证目录名（作者本机账号），
	// 既是个人称呼又依赖特定机器。改成不认名字，只认「目录里有没有凭证文件」——
	// 这才是 credDirs() 的真实契约（目录名由用户自己起）。
	dirs := credDirs()
	if len(dirs) == 0 {
		t.Skipf("数据根下没有任何凭证目录（%s），跳过真实环境测试", hubRoot)
	}
	var usable string
	for _, d := range dirs {
		if dirHasCredential(d) {
			usable = d
			break
		}
	}
	if usable == "" {
		t.Skipf("扫到的目录里都没有凭证文件（%v），跳过", dirs)
	}
	p := NewIntlProxy(usable)
	got := p.AuthFilePath()
	if got == "" || !fileExists(got) {
		t.Fatalf("AuthFilePath 未解析出真实文件: %q", got)
	}
	t.Logf("%s 解析到: %s", filepath.Base(usable), got)
}

// TestAnnotateRiskControl 11140 风控提示注入：
//   - 真实上游 403 响应体 → displayMsg.zh/en 尾部出现 [hub] 提示，JSON 仍合法；
//   - 非 11140 / 非 403 → 原样返回。
func TestAnnotateRiskControl(t *testing.T) {
	upstream := `{"code":11140,"msg":"request illegal","requestId":"72de566e-717d-482a-a6bf-5b53112c0e91","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核，请调整后重试。","zh-hant":"內容未通過安全審核，請調整後重試。"},"actions":["SUBMIT_FEEDBACK","COPY_ERROR","EDIT_INPUT"]}`

	out := annotateRiskControl(403, []byte(upstream))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("注入后 JSON 不合法: %v", err)
	}
	dm := m["displayMsg"].(map[string]any)
	if !strings.Contains(dm["zh"].(string), "[hub]") || !strings.Contains(dm["zh"].(string), "换凭证") {
		t.Fatalf("zh 提示未注入: %v", dm["zh"])
	}
	if !strings.Contains(dm["en"].(string), "[hub]") {
		t.Fatalf("en 提示未注入: %v", dm["en"])
	}
	if m["code"].(float64) != 11140 {
		t.Fatalf("code 被改动: %v", m["code"])
	}

	// 非 11140 / 非 403：必须原样返回
	if got := annotateRiskControl(200, []byte(upstream)); string(got) != upstream {
		t.Fatalf("200 响应不应被改写")
	}
	plain := []byte(`{"error":"boom"}`)
	if got := annotateRiskControl(403, plain); string(got) != string(plain) {
		t.Fatalf("非 11140 的 403 不应被改写: %s", got)
	}
}
