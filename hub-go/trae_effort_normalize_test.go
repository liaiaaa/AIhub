package main

// 2026-11-03：trae 客户端档位归一化（修「发 max 静默降档」）。

import "testing"

func TestTraeNormalizeClientEffort(t *testing.T) {
	cases := []struct {
		name  string
		model string
		in    any
		want  string // 期望写回 raw 的值
		gone  bool   // 期望整个键被删掉
	}{
		{"规范名 max → extra_high", "deepseek-v4.1-flash", "max", "extra_high", false},
		{"规范名 medium → high", "deepseek-v4.1-flash", "medium", "high", false},
		{"规范名 minimal → light", "deepseek-v4.1-flash", "minimal", "light", false},
		{"原生名原样保留", "deepseek-v4.1-flash", "high", "high", false},
		{"大小写不敏感", "deepseek-v4.1-flash", "MAX", "extra_high", false},
		{"两档模型：max 钳到 high", "Doubao-Seed-2.1-Turbo", "max", "high", false},
		{"不支持思考的模型：删掉", "mimo-v2.6-flash", "max", "", true},
		{"空值：删掉", "deepseek-v4.1-flash", "", "", true},
	}
	for _, c := range cases {
		raw := map[string]any{"reasoning_effort": c.in}
		got := traeNormalizeClientEffort(raw, c.model)
		if c.gone {
			if _, ok := raw["reasoning_effort"]; ok {
				t.Errorf("%s：应删除键，实际 %v（返回 %q）", c.name, raw["reasoning_effort"], got)
			}
			continue
		}
		if raw["reasoning_effort"] != c.want || got != c.want {
			t.Errorf("%s：期望 %q，实际 raw=%v 返回=%q", c.name, c.want, raw["reasoning_effort"], got)
		}
	}
}

func TestTraeNormalizeKeepsDefaultInjection(t *testing.T) {
	// 客户端没给档位：归一化不应插手，交给 traeApplyDefaultEffort 补默认
	raw := map[string]any{}
	if got := traeNormalizeClientEffort(raw, "deepseek-v4.1-flash"); got != "" {
		t.Errorf("没给档位时不该返回值，实际 %q", got)
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("没给档位时不该写入")
	}
	if eff := traeApplyDefaultEffort(raw, "deepseek-v4.1-flash"); eff != "extra_high" {
		t.Errorf("默认应注入该模型最高档 extra_high，实际 %q", eff)
	}
}
