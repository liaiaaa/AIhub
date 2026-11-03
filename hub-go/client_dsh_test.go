package main

// 2026-11-03：DSH 接管（只改 providers.aihub.models 一段，其余不动）。

import (
	"strings"
	"testing"
)

const dshProfileFixture = `- id: llm-pi-ai
  name: "@deepseek-ai/dsh-llm-pi-ai"
  config:
    providers:
      aihub:
        displayName: AIHUB
        baseURL: http://127.0.0.1:8790/v1
        models:
          - id: old.model
            name: "旧模型"
            contextWindow: 1000
      other:
        displayName: 别家
        models:
          - id: keep.me
            name: "别家模型"
- id: agent-default-model
  config:
    model: aihub/intl.deepseek-v4.1-flash
`

func TestReplaceAihubModelsBlock(t *testing.T) {
	gen := strings.TrimSuffix(dshModelsYAML([]dshPiModel{
		{ID: "intl.deepseek-v4.1-flash", Name: "DeepSeek 4.1 Flash（国际）", ContextWindow: 1000000,
			MaxTokens: 128000, Input: []string{"text", "image"},
			ReasoningEfforts: map[string]string{"low": "low", "high": "high", "max": "max"}},
		{ID: "teleagent.chat-pro", Name: "chat-pro（TeleAgent）", ContextWindow: 1000000,
			MaxTokens: 65536, Input: []string{"text", "image"},
			ReasoningEfforts: map[string]string{"low": "low", "high": "high", "max": "max"}},
	}, "  "), "\n")

	out, err := replaceAihubModelsBlock(dshProfileFixture, gen)
	if err != nil {
		t.Fatalf("替换失败：%v", err)
	}
	// 1) 旧模型没了
	if strings.Contains(out, "old.model") {
		t.Error("旧的 models 条目应被替换掉")
	}
	// 2) 新模型在
	for _, want := range []string{"- id: \"intl.deepseek-v4.1-flash\"", "contextWindow: 1000000", "reasoningEfforts:"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q", want)
		}
	}
	// 3) 别家 provider 与默认模型一个字都不能动
	if !strings.Contains(out, "keep.me") {
		t.Error("其他 provider 的模型被误删了")
	}
	if !strings.Contains(out, "model: aihub/intl.deepseek-v4.1-flash") {
		t.Error("agent-default-model 段被误改")
	}
	// 4) 缩进必须正确（models: 下 2 空格起条目）
	if !strings.Contains(out, "\n          - id: ") {
		t.Errorf("条目缩进不对，实际片段：\n%s", excerpt(out, "models:", 6))
	}
	// 5) 结果里只能有一个 models: 属于 aihub 段（别家的 models: 保留）
	if strings.Count(out, "        models:") != 2 {
		t.Errorf("应保留两个 models:（aihub 与 other 各一个），实际 %d", strings.Count(out, "        models:"))
	}
}

func TestReplaceAihubModelsBlockErrors(t *testing.T) {
	if _, err := replaceAihubModelsBlock("- id: x\n", "  - id: y\n"); err == nil {
		t.Error("没有 providers 段时应报错，而不是乱改")
	}
	noAihub := "  config:\n    providers:\n      other:\n        models:\n          - id: a\n"
	if _, err := replaceAihubModelsBlock(noAihub, "  - id: y\n"); err == nil {
		t.Error("没有 aihub 块时应报错")
	}
}

func TestDSHEffortsMapHidesUnsupported(t *testing.T) {
	// trae 只有 low/high/max：其余档位不该出现（DSH 会把没写的档位当成可选）
	m := dshEffortsMap([]string{"low", "high", "max"})
	if len(m) != 3 || m["max"] != "max" || m["low"] != "low" {
		t.Errorf("映射不对：%v", m)
	}
	if _, ok := m["medium"]; ok {
		t.Error("模型不支持的 medium 不该写进去")
	}
	if dshEffortsMap(nil) != nil {
		t.Error("无档位模型应返回 nil（不写 reasoningEfforts）")
	}
}

func TestYAMLScalarQuotesChinese(t *testing.T) {
	if got := yamlScalar("DeepSeek 4.1 Flash（国际） · 上下文 1M"); !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("含中文/特殊符号的显示名应加引号：%s", got)
	}
}

// excerpt 取包含 anchor 的一段文本，便于失败时看现场。
func excerpt(s, anchor string, lines int) string {
	idx := strings.Index(s, anchor)
	if idx < 0 {
		return s
	}
	part := strings.Split(s[idx:], "\n")
	if len(part) > lines {
		part = part[:lines]
	}
	return strings.Join(part, "\n")
}

// 合并式同步：目录里没有的模型必须原样保留。
// 真实事故（2026-11-03）：第一次同步把 intl.deepseek-v4.1-flash 删了，
// 而 agent-default-model 正指向它。
