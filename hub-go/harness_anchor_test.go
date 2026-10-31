package main

// 2026-10-31：补两类此前没覆盖的 11128 触发源。
//
// 来源不是猜的：dsh 内核注入的两段原文——
//  1. @deepseek-ai/dsh-file-reference 的 FILE_REFERENCE_PROMPT，整段以
//     「Tokens prefixed with @」开头，段内有字面量 @；
//  2. @deepseek-ai/dsh-harness-dev-context 的 WEB_SURFACE_PROMPT
//     「You are interacting with the user through the DeepSeek Harness Web GUI …」
//     以及同一插件注册的运行时上下文「Current DeepSeek Harness Web GUI URL: …」。
//
// 这两段都不带「You are … powered by …」结构，所以此前的 harnessIdentityRe /
// harnessPoweredByRe 一条都匹配不到，会原样发给上游。
//
// 约束与身份句一致：只清这两段的指纹，不动用户正文里自己的 @ 路径。

import (
	"strings"
	"testing"
)

// dshFileReferenceSection 是内核原文（dsh 当前版本，逐字取自 system prompt）。
const dshFileReferenceSection = "Tokens prefixed with @ are paths the user explicitly referenced. " +
	"Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. " +
	"A trailing slash marks a directory: list it when its contents matter. " +
	"Anything else is a file: use the read tool when its contents are needed, " +
	"and do not claim to have inspected it before reading. " +
	"@\"...\" quotes a path containing spaces."

func TestSanitizeNeutralizesFileReferenceAnchor(t *testing.T) {
	in := "You are a helpful AI assistant.\n\n" + dshFileReferenceSection + "\n\nAgent Teams is available in this session"

	got := sanitizeHarnessPrompt(in)

	if strings.Contains(got, "Tokens prefixed with @") {
		t.Errorf("文件引用段的 @ 锚点没清掉：%q", got)
	}
	if !strings.Contains(got, "Tokens prefixed with (at)") {
		t.Errorf("应改成 (at) 写法，实际：%q", got)
	}
	// 段内另一处 @ 也在网关判据里
	if strings.Contains(got, "@\"...\"") {
		t.Errorf("段内 @\"...\" 没清掉：%q", got)
	}
	// 功能性内容必须原样保留
	if !strings.Contains(got, "Agent Teams is available in this session") {
		t.Errorf("功能性内容被破坏：%q", got)
	}
	if !strings.Contains(got, "You are a helpful AI assistant.") {
		t.Errorf("中性身份句被破坏：%q", got)
	}
}

// 用户正文里的 @ 路径是真实含义，不能被换掉（越权改写）。
func TestSanitizeKeepsUserAtPathsOutsideAnchor(t *testing.T) {
	in := "请读 @hub-go/main.go 里的 sanitizeHarnessPrompt，别动 @docs/README.md"
	if got := sanitizeHarnessPrompt(in); got != in {
		t.Errorf("用户正文里的 @ 路径被改：%q → %q", in, got)
	}
}

func TestSanitizeNeutralizesWebSurfaceSentence(t *testing.T) {
	in := "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	got := sanitizeHarnessPrompt(in)

	if strings.Contains(got, "DeepSeek Harness Web GUI") {
		t.Errorf("Web GUI 身份句没清掉：%q", got)
	}
	if !strings.Contains(got, "this Web GUI") {
		t.Errorf("应改成不含品牌的说法，实际：%q", got)
	}
}

// 内核把零宽空格夹在 DeepSeek 与 Harness 之间；正则必须容忍它，否则漏判。
func TestSanitizeNeutralizesWebSurfaceWithZeroWidthSpace(t *testing.T) {
	in := "You are interacting with the user through the DeepSeek\u200B Harness Web GUI whose URL the runtime context names."
	got := sanitizeHarnessPrompt(in)

	if strings.Contains(got, "Harness Web GUI") {
		t.Errorf("带 U+200B 的 Web GUI 句没清掉：%q", got)
	}
}

func TestSanitizeNeutralizesCurrentWebGUIURL(t *testing.T) {
	in := "Current DeepSeek Harness Web GUI URL: \"http://127.0.0.1:39321\"."
	got := sanitizeHarnessPrompt(in)

	if strings.Contains(got, "DeepSeek") {
		t.Errorf("URL 行的品牌指纹没清掉：%q", got)
	}
	// URL 本身是真实地址，必须保留
	if !strings.Contains(got, "http://127.0.0.1:39321") {
		t.Errorf("URL 被破坏：%q", got)
	}
	if !strings.Contains(got, "Current Web GUI URL") {
		t.Errorf("应改成中性说法，实际：%q", got)
	}
}

// 兜底：残留的「DeepSeek Harness」品牌连写也要去掉（含各种不可见分隔）。
func TestSanitizeNeutralizesBrandFallback(t *testing.T) {
	cases := []string{
		"Current DeepSeek Harness implementation checkout: \"/home/<user>/.local/lib/node_modules\"",
		"The runtime context names the DeepSeek\u200B Harness implementation checkout.",
	}
	for _, in := range cases {
		got := sanitizeHarnessPrompt(in)
		if strings.Contains(got, "DeepSeek Harness") || strings.Contains(got, "DeepSeek\u200B Harness") {
			t.Errorf("品牌兜底没生效：%q → %q", in, got)
		}
		if !strings.Contains(got, "implementation checkout") {
			t.Errorf("功能性内容被破坏：%q", got)
		}
	}
}

// 真实 system prompt 是多段指纹拼在一起的，必须一次全清，且幂等。
func TestSanitizeNeutralizesAllAnchorsTogetherAndIsIdempotent(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek\u200B Harness.\n\n" +
		"You are a helpful AI assistant.\n\n" +
		dshFileReferenceSection + "\n\n" +
		"You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names.\n\n" +
		"Current DeepSeek Harness Web GUI URL: \"http://127.0.0.1:39321\".\n\n" +
		"Current DeepSeek Harness implementation checkout: \"/home/<user>/.local/lib/node_modules/@deepseek-ai/dsh/\"."

	got := sanitizeHarnessPrompt(in)

	for _, bad := range []string{"DeepSeek Harness", "DeepSeek\u200B Harness", "Tokens prefixed with @", "Harness Web GUI"} {
		if strings.Contains(got, bad) {
			t.Errorf("仍有指纹 %q：%q", bad, got)
		}
	}
	// 功能内容全在
	for _, must := range []string{"use the read tool", "http://127.0.0.1:39321", "implementation checkout"} {
		if !strings.Contains(got, must) {
			t.Errorf("功能性内容 %q 丢失：%q", must, got)
		}
	}

	// 幂等：清洗过的文本再清洗必须不变（否则多路径/重试会反复改）
	if twice := sanitizeHarnessPrompt(got); twice != got {
		t.Errorf("清洗不幂等：%q → %q", got, twice)
	}
}
