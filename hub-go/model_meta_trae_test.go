package main

// 2026-11-01：Trae 线路的模型元数据回归锁定。
//
// 起因：用户问「dsh 的模型表单里，trae.deepseek-v4.1-flash 的最大输出 token 为什么
// 不自动填」。查下来是 aihub 这边的问题 —— modelMeta() 没有 trae 分支、
// staticModelMeta 也没有 trae 分组，于是 /v1/models 对 trae 的模型只回
// id/name/object/created/owned_by，客户端拿不到 contextWindow / max_output_tokens。
//
// 数值来源（不是猜的）：Trae CN 桌面端缓存的模型目录
// `~/.config/Trae CN/User/globalStorage/state.vscdb` 里
// `AI.agent.model.model_list_map` → `solo_agent` 段：
//   context_window_tokens={dev:200000,max:1000000}, prompt_max_tokens=936000,
//   max_tokens=64000（936000+64000=1000000 自证语义）。
//
// 这两个测试不碰网络、不读真实凭证，只钉住「表里有 trae」和「/v1/models 会带字段」。

import "testing"

func TestTraeModelMeta(t *testing.T) {
	h := &Hub{}
	for _, model := range []string{"deepseek-v4.1-flash", "glm-5.3", "kimi-k3"} {
		m, ok := h.modelMeta("trae", model)
		if !ok {
			t.Errorf("trae/%s 查不到元数据 —— staticModelMeta 里的 trae 分组丢了？", model)
			continue
		}
		if m.ContextWindow != 1000000 {
			t.Errorf("trae/%s 上下文窗口 = %d，期望 1000000", model, m.ContextWindow)
		}
		if m.MaxInput != 64000 {
			t.Errorf("trae/%s 输出上限 = %d，期望 64000", model, m.MaxInput)
		}
		// 思考档位上游未公开，必须保持空（default_thinking.go 不注入 Trae 档位）
		if len(m.Efforts) != 0 {
			t.Errorf("trae/%s 不该有 Efforts，实际 %v", model, m.Efforts)
		}
	}
	// 逐模型真值（Trae 目录里各模型上限并不一样，别拿一个值套全部）
	for _, c := range []struct {
		model string
		ctx   int
		out   int
	}{
		{"Doubao-Seed-2.1-Turbo", 256000, 32000}, // 小窗口模型
		{"mimo-v2.6-flash", 981504, 64000},       // 非整 1M
		{"glm-5.3-flashx", 1000000, 64000},
		{"qwen3.8-max", 1000000, 64000},
		{"step-5-preview", 1000000, 64000},
	} {
		m, ok := h.modelMeta("trae", c.model)
		if !ok {
			t.Errorf("trae/%s 查不到元数据", c.model)
			continue
		}
		if m.ContextWindow != c.ctx || m.MaxInput != c.out {
			t.Errorf("trae/%s = %d/%d，期望 %d/%d", c.model, m.ContextWindow, m.MaxInput, c.ctx, c.out)
		}
	}
	// 没登记的模型仍应查不到（宁缺不编）：老模型不在当前目录里，就不该编数字
	for _, model := range []string{"不存在的模型", "glm-5.2", "DeepSeek-V4-Flash"} {
		if _, ok := h.modelMeta("trae", model); ok {
			t.Errorf("未登记元数据的 trae 模型不该返回（%s）", model)
		}
	}
}

// TestTraeModelsListMatchesUpstream 钉住「清单与上游目录 + 实测结论一致」。
func TestTraeModelsListMatchesUpstream(t *testing.T) {
	has := map[string]bool{}
	for _, m := range TraeModels {
		if has[m] {
			t.Errorf("TraeModels 里有重复项：%s", m)
		}
		has[m] = true
	}
	// 2026-11-04：剔除 3 个上游已关停的（Doubao-Seed-Code / kimi-k3.1-pro / qwen3.8-flash），28 → 25
	if len(TraeModels) != 25 {
		t.Errorf("TraeModels 应为 25 个，实际 %d", len(TraeModels))
	}
	// 上游目录里有、旧清单缺的那批，必须补进来
	for _, want := range []string{
		"glm-5.3-flash", "glm-5.3-flashx", "qwen3.8-max",
		"mimo-v2.6-flash", "mimo-v2.6-pro", "step-5-preview", "minimax-m3.1-flash",
		"Doubao-Seed-Evolving", "DeepSeek-V4-Pro-Official",
	} {
		if !has[want] {
			t.Errorf("TraeModels 缺少上游可用模型：%s", want)
		}
	}
	// 实测上游报错的必须剔除
	for _, bad := range []string{"custom_model_gemini", "custom_model_deepseek_reasoner",
		"Doubao-Seed-Code", "kimi-k3.1-pro", "qwen3.8-flash"} {
		if has[bad] {
			t.Errorf("TraeModels 不该包含实测报错的模型：%s", bad)
		}
	}
}

func TestAggModelsTraeCarriesLimits(t *testing.T) {
	h := &Hub{}
	aggMu.Lock()
	saved := aggList
	aggList = []aggModel{
		{Line: "trae", Slug: "trae.deepseek-v4.1-flash", Backend: "deepseek-v4.1-flash",
			Display: "DeepSeek 4.1 Flash（Trae）"},
	}
	aggMu.Unlock()
	defer func() { aggMu.Lock(); aggList = saved; aggMu.Unlock() }()

	resp := h.aggOpenAIModels()
	data, _ := resp["data"].([]map[string]any)
	if len(data) != 1 {
		t.Fatalf("应有 1 个模型，实际 %d", len(data))
	}
	e := data[0]

	// max_output_tokens：dsh-llm-deepseek 与 pi-ai 都在读的字段名
	if got, _ := e["max_output_tokens"].(int); got != 64000 {
		t.Errorf("max_output_tokens = %v，期望 64000", e["max_output_tokens"])
	}
	// context_window（snake）与 contextWindow（camel）两种写法都要在：
	// 前者是 DSH deepseek provider 认的名字，后者是早期自定义客户端用的。
	if got, _ := e["context_window"].(int); got != 1000000 {
		t.Errorf("context_window = %v，期望 1000000", e["context_window"])
	}
	if got, _ := e["contextWindow"].(int); got != 1000000 {
		t.Errorf("contextWindow = %v，期望 1000000", e["contextWindow"])
	}
}
