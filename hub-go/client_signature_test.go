package main

// 2026-10-21：上游 WorkBuddy 渠道校验不只验请求头，还认 system prompt 里的
// 第三方客户端签名短语。同一份真实失败 dump 重复采样的稳定结果：
//
//	原 system                                            → 11128 拦截 3/3
//	只留第 1 段「You are an AI agent powered by
//	  DeepSeek Harness.」（48 字符）                      → 11128 拦截 3/3
//	去掉第 1 段                                          → 通过 2/2
//	「You are an AI agent.」                             → 通过 2/2
//	「You are an AI agent powered by X.」                 → 通过 2/2
//	「You are a helpful assistant powered by DeepSeek
//	  Harness.」                                         → 通过 2/2
//
// 即触发条件是**完整短语**，不是单词黑名单。这组测试钉住清洗行为。

import (
	"strings"
	"testing"
)

const dshSignature = "You are an AI agent powered by DeepSeek Harness."

func TestSanitizeRemovesDshSignature(t *testing.T) {
	sys := dshSignature + "\n\nYou are a coding agent powered by the dom.step-5-preview model.\n\nAgent Teams is available."
	msgs := []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hi"},
	}
	out := sanitizeHarnessMessages(msgs)
	got := out[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "DeepSeek Harness") {
		t.Errorf("签名没清掉：%q", got)
	}
	if !strings.HasPrefix(got, "You are an AI agent.") {
		t.Errorf("应替换成中性表述，实际开头：%q", got[:40])
	}
	// 功能性内容必须原样保留 —— 改写提示词会改变模型行为
	if !strings.Contains(got, "dom.step-5-preview") || !strings.Contains(got, "Agent Teams is available") {
		t.Errorf("功能性内容被破坏：%q", got)
	}
	// 其它消息不动
	if out[1].(map[string]any)["content"] != "hi" {
		t.Error("user 消息不该被改")
	}
}

// 用户消息里出现同样短语不能动（越权改用户内容）
func TestSanitizeLeavesUserMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are a helpful assistant."},
		map[string]any{"role": "user", "content": "请解释这句话：" + dshSignature},
	}
	out := sanitizeHarnessMessages(msgs)
	if !strings.Contains(out[1].(map[string]any)["content"].(string), "DeepSeek Harness") {
		t.Error("用户消息被改写了，这属于越权")
	}
}

// 无签名时不该 clone（零开销路径），返回原切片
func TestSanitizeNoopReturnsSameSlice(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are a helpful assistant."},
	}
	out := sanitizeHarnessMessages(msgs)
	if len(out) != 1 {
		t.Fatalf("消息数变了：%d", len(out))
	}
	if out[0].(map[string]any)["content"] != "You are a helpful assistant." {
		t.Error("没有签名时不该改动内容")
	}
}

// 非字符串 content（多模态段数组）不能崩
func TestSanitizeHandlesNonStringContent(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": dshSignature},
		}},
	}
	out := sanitizeHarnessMessages(msgs)
	if len(out) != 1 {
		t.Fatal("消息数变了")
	}
	// 数组形态当前不处理（签名只在纯文本 system 里出现），但不能 panic/丢消息
	if _, ok := out[0].(map[string]any)["content"].([]any); !ok {
		t.Error("数组 content 被改坏了")
	}
}

// 两类已实测的签名都要被清掉（2026-10-10 Codex 的 OpenAI、
// 2026-10-21 dsh 的开场白），而且清洗后不能再留下签名特征。
func TestSanitizeCoversBothKnownRules(t *testing.T) {
	// 规则 1：Codex
	if got := sanitizeHarnessPrompt("You are Codex, based on OpenAI models."); strings.Contains(got, "OpenAI") {
		t.Errorf("Codex 的 OpenAI 签名没清掉：%q", got)
	}
	// 规则 2：dsh
	got := sanitizeHarnessPrompt(dshSignature)
	if strings.Contains(got, "DeepSeek Harness") {
		t.Errorf("dsh 签名没清掉：%q", got)
	}
	if got != "You are an AI agent." {
		t.Errorf("应替换成中性表述，实际：%q", got)
	}
	// 正常内容不能被误伤
	if got := sanitizeHarnessPrompt("You are a helpful assistant."); got != "You are a helpful assistant." {
		t.Errorf("正常内容被改了：%q", got)
	}
}
