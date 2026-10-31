package main

// 出站身份守卫：同一账号在不同接口上自报不同客户端版本，是最容易被上游识别
// 成「非官方客户端」的特征（2026-10-25 统一修）。这组测试钉住两件事：
//
//  1. 所有线路、所有非对话请求的 UserAgent 都来自官方常量，不再各写各的；
//  2. 非测试源码里不允许再出现旧的自报家门字面量。
//     第二项用 go/ast 查**字符串字面量**，注释里的历史记录不算——否则
//     代码注释里写「旧值 codebuddy2openai/2.0」都会让测试误报。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOfficialIdentityConstants(t *testing.T) {
	want := map[string]string{
		"dom":     "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0",
		"intl":    "WorkBuddy/5.8.4 WorkBuddy AI/5.8.4 CLI/2.161.0",
		"billing": "WorkBuddy/5.8.3",
		"catalog": "CLI/2.161.0 CodeBuddy/2.161.0",
	}
	if got := officialDomUA(); got != want["dom"] {
		t.Errorf("officialDomUA = %q，应为 %q", got, want["dom"])
	}
	if got := officialIntlUA(); got != want["intl"] {
		t.Errorf("officialIntlUA = %q，应为 %q", got, want["intl"])
	}
	if got := officialDomBillingUA(); got != want["billing"] {
		t.Errorf("officialDomBillingUA = %q，应为 %q", got, want["billing"])
	}
	if got := officialCLICodeBuddyUA(); got != want["catalog"] {
		t.Errorf("officialCLICodeBuddyUA = %q，应为 %q", got, want["catalog"])
	}
}

func TestProxyUserAgentsAreOfficial(t *testing.T) {
	if got := NewDomProxyDir("").UserAgent; got != officialDomUA() {
		t.Errorf("dom 线路 UA = %q，应取官方常量", got)
	}
	if got := NewIntlProxy("").UserAgent; got != officialIntlUA() {
		t.Errorf("intl 线路 UA = %q，应取官方常量", got)
	}
	// 对话路径自己会覆盖 UA；两条路径必须报同一个值，不能一个官方一个旧串。
	h := officialChatHeaders(map[string]string{})
	if h["User-Agent"] != NewDomProxyDir("").UserAgent {
		t.Errorf("对话头 UA %q 与线路 UA %q 不一致", h["User-Agent"], NewDomProxyDir("").UserAgent)
	}
	if h["Accept"] != "application/json, text/plain, */*" {
		t.Errorf("对话头 Accept = %q", h["Accept"])
	}
}

// 非对话请求（签到 / 探活 / 目录 / billing）走 CredentialManager.buildHeaders，
// 这里钉住它们不再发 application/json 这种可区分的窄 Accept。
func TestCredentialHeadersUseOfficialAccept(t *testing.T) {
	cm := &CredentialManager{defaultDomain: "www.codebuddy.cn", userAgent: officialDomUA()}
	h := cm.buildHeaders(
		&AuthData{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		&AccountData{UID: "u1", EnterpriseID: "e1"},
	)
	if got := h["Accept"]; got != "application/json, text/plain, */*" {
		t.Errorf("Accept = %q，应与官方客户端一致", got)
	}
	if got := h["User-Agent"]; got != officialDomUA() {
		t.Errorf("User-Agent = %q，应取官方常量", got)
	}
}

func TestNoLegacyIdentityLiteralsInSource(t *testing.T) {
	legacy := []string{
		"codebuddy2openai", // 旧 dom UA：直接自报是第三方转换器
		"codebuddy-guoji",  // 旧 intl UA：同上
		"WorkBuddy/5.5.",   // 旧 billing UA 版本
		"WorkBuddy/5.7.6",  // 旧目录 UA 版本
		"CLI/2.63",         // 旧目录 CLI 版本
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			for _, old := range legacy {
				if strings.Contains(v, old) {
					t.Errorf("%s:%d 仍存在旧身份字面量 %q（%s）",
						f, fset.Position(bl.Pos()).Line, old, v)
				}
			}
			return true
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("没扫到任何源码文件，测试形同虚设")
	}
}
