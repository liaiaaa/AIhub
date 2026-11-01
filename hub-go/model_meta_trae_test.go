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
	// 没登记的模型仍应查不到（宁缺不编）
	if _, ok := h.modelMeta("trae", "不存在的模型"); ok {
		t.Error("未登记的 trae 模型不该返回元数据")
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
