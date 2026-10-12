package main

// Zcode（Z.ai）模型目录（单一事实源，内置快照）。
//
// 与 Qoder 目录的区别：Z.ai 没有公开的 GET /models 目录接口（docs.z.ai 未提及，
// 实测路径也不通），因此只有内置快照；未收录的模型 id 原样透传给上游。
//
// 能力数据来自官方文档（docs.z.ai/guides/llm/*，2026-09-26 核对）：
//   - glm-5.3        纯文本；思考不可关闭（thinking.type 仅 enabled），
//                    深度用 reasoning_effort = low|high|max 控制（默认 max）
//   - glm-5.3-flash  视觉（Video/Image/Text/File）；思考不可关闭；推荐 effort=max
//   - glm-5.3-flashx flash 的加速变体，能力画像同 flash
//   - glm-5.2        纯文本；thinking.type 可 enabled/disabled（默认 enabled）；
//                    文档示例 reasoning_effort 均为 max
//   - glm-4.6        上一代，thinking.type 可开关，200K 上下文
//
// 官方迁移指引：5.3 系此前 thinking.type:"disabled" 的用法应改为
// "enabled" + reasoning_effort:"low" —— 本代理的关闭映射遵循该指引。

import "strings"

// zcodeModelEntry 目录中的一个模型。
type zcodeModelEntry struct {
	Key         string `json:"key"`          // 上游真实模型 id
	DisplayName string `json:"display_name"` // 对外显示名（别名）
	Vendor      string `json:"vendor"`
	IsVision    bool   `json:"is_vision"`
	IsReasoning bool   `json:"is_reasoning"` // 是否思考模型
	IsDefault   bool   `json:"is_default"`
	// SupportsDisable 思考能否整体关闭（5.3 系 false：只能调档不能关）。
	SupportsDisable bool `json:"supports_disabled,omitempty"`
	// Efforts 该模型支持的 reasoning_effort 档（规范序子集）；空 = 不发档位。
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	// MaxInputTokens / MaxOutputTokens 上下文与输出上限（token）。
	MaxInputTokens  int      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	ContextWindows  []string `json:"context_windows,omitempty"`
}

// zcodeDefaultCatalog 内置快照（能力按官方文档与 pi-zcode-provider 参考实现核对）。
func zcodeDefaultCatalog() []zcodeModelEntry {
	return []zcodeModelEntry{
		{Key: "glm-5.3", DisplayName: "GLM-5.3", Vendor: "zai", IsReasoning: true,
			IsDefault: true, SupportsDisable: false,
			Efforts: []string{"low", "high", "max"}, DefaultEffort: "max",
			MaxInputTokens: 1000000, MaxOutputTokens: 131072, ContextWindows: []string{"1M"}},
		{Key: "glm-5.3-flash", DisplayName: "GLM-5.3-Flash", Vendor: "zai",
			IsVision: true, IsReasoning: true, SupportsDisable: false,
			Efforts: []string{"low", "high", "max"}, DefaultEffort: "max",
			MaxInputTokens: 1000000, MaxOutputTokens: 131072, ContextWindows: []string{"1M"}},
		{Key: "glm-5.3-flashx", DisplayName: "GLM-5.3-FlashX", Vendor: "zai",
			IsVision: true, IsReasoning: true, SupportsDisable: false,
			Efforts: []string{"low", "high", "max"}, DefaultEffort: "max",
			MaxInputTokens: 1000000, MaxOutputTokens: 131072, ContextWindows: []string{"1M"}},
		// glm-5.2：thinking 可开关；参考实现的档位映射 low/medium/high→"high"、max→"max"
		{Key: "glm-5.2", DisplayName: "GLM-5.2", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true, Efforts: []string{"high", "max"}, DefaultEffort: "",
			MaxInputTokens: 1000000, MaxOutputTokens: 131072, ContextWindows: []string{"1M"}},
		{Key: "glm-5.1", DisplayName: "GLM-5.1", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true,
			MaxInputTokens:  204800, MaxOutputTokens: 131072, ContextWindows: []string{"200K"}},
		{Key: "glm-5-turbo", DisplayName: "GLM-5-Turbo", Vendor: "zai",
			SupportsDisable: true,
			MaxInputTokens:  200000, MaxOutputTokens: 131072, ContextWindows: []string{"200K"}},
		{Key: "glm-5", DisplayName: "GLM-5", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true,
			MaxInputTokens:  204800, MaxOutputTokens: 131072, ContextWindows: []string{"200K"}},
		{Key: "glm-4.7", DisplayName: "GLM-4.7", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true,
			MaxInputTokens:  204800, MaxOutputTokens: 131072, ContextWindows: []string{"200K"}},
		{Key: "glm-4.6", DisplayName: "GLM-4.6", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true,
			MaxInputTokens:  204800, MaxOutputTokens: 131072, ContextWindows: []string{"200K"}},
		{Key: "glm-4.5", DisplayName: "GLM-4.5", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true,
			MaxInputTokens:  131072, MaxOutputTokens: 131072, ContextWindows: []string{"131K"}},
		{Key: "glm-4.5-air", DisplayName: "GLM-4.5-Air", Vendor: "zai", IsReasoning: true,
			SupportsDisable: true,
			MaxInputTokens:  131072, MaxOutputTokens: 131072, ContextWindows: []string{"131K"}},
	}
}

// zcodeModelAllowlist 对外目录白名单：当前所有套餐（Start Plan / 活动赠送）
// 的 entitlement 只覆盖 glm-5.3 与 glm-5.3-flash，其余模型没有可用额度，
// 挂出来只会误导客户端选择。换套餐（如 Individual Coding Plan）后按需增删。
// 注意：仅约束 /v1/models 列表与计数；目录内部保留全量快照，
// 显式请求未收录模型仍原样透传上游（thinking 元数据照常生效）。
var zcodeModelAllowlist = map[string]bool{
	"glm-5.3":       true,
	"glm-5.3-flash": true,
}

// zcodeFilterCatalog 按白名单过滤（保持原有顺序）。
func zcodeFilterCatalog(entries []zcodeModelEntry) []zcodeModelEntry {
	out := make([]zcodeModelEntry, 0, 2)
	for _, e := range entries {
		if zcodeModelAllowlist[e.Key] {
			out = append(out, e)
		}
	}
	return out
}

// zcodeDefaultKey 默认模型：glm-5.3。
func zcodeDefaultKey(catalog []zcodeModelEntry) string {
	for _, m := range catalog {
		if m.IsDefault {
			return m.Key
		}
	}
	if len(catalog) > 0 {
		return catalog[0].Key
	}
	return "glm-5.3"
}

// buildZcodeAliases 构建「别名 → 真实 key」映射（真实 key 与显示名双轨）。
func buildZcodeAliases(catalog []zcodeModelEntry) map[string]string {
	aliases := make(map[string]string, len(catalog)*3)
	add := func(alias, key string) {
		n := qoderNormalizeAlias(alias) // 归一规则通用：小写、去空白、下划线转连字符
		if n == "" {
			return
		}
		if _, exists := aliases[n]; !exists {
			aliases[n] = key
		}
	}
	for _, m := range catalog {
		add(m.Key, m.Key)
	}
	for _, m := range catalog {
		add(m.DisplayName, m.Key)
	}
	// 人工别名：常见叫法
	for _, extra := range []struct{ alias, key string }{
		{"glm5.3", "glm-5.3"},
		{"glm5.3-flash", "glm-5.3-flash"},
		{"glm-5.3-flash-x", "glm-5.3-flashx"},
		{"glm5.2", "glm-5.2"},
		{"glm-5.2", "glm-5.2"},
		{"default", "glm-5.3"},
	} {
		add(extra.alias, extra.key)
	}
	return aliases
}

// zcodeEntryFor 按真实 key 查目录条目（未收录返回 nil）。
func zcodeEntryFor(catalog []zcodeModelEntry, key string) *zcodeModelEntry {
	for i := range catalog {
		if catalog[i].Key == key {
			return &catalog[i]
		}
	}
	return nil
}

// zcodeVendorFor 从显示名推断厂商（仅目录兜底用）。
func zcodeVendorFor(display string) string {
	if strings.HasPrefix(strings.ToLower(display), "glm") {
		return "zai"
	}
	return "zai"
}
