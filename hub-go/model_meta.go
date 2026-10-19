package main

// 模型元数据（2026-10-13 用户要求）：上下文窗口 / 输入上限 / 思考强度档位。
// 来源：
//   - qoder / zcode：官方目录实时刷新自带（qoder_models.go / zcode_models.go）；
//   - dom / intl：实测表（2026-10 实测：11115 错误确认国际 DS-V4.1-Flash 1M
//     硬顶；README 档位表记录了各模型实测 reasoning_efforts）；
//     未实测的模型不填（不瞎编）。
// 展示在探测结果、网关目录、模型下拉的副文本里。

import (
	"strconv"
	"strings"
)

// modelMeta 单模型元数据（0 值/空切片 = 无数据，不展示）。
type modelMeta struct {
	ContextWindow int
	MaxInput      int
	Efforts       []string
}

// staticModelMeta dom/intl 实测元数据（未列出的模型 = 无可靠数据）。
var staticModelMeta = map[string]map[string]modelMeta{
	"dom": {
		// 2026-10-15 依 倍率/workbuddy国内.md + workbuddy国际.md 补上下文窗口。
		// 表里上下文一列写「1M/32–128K」这类入/出区间：入取上限 1M；出取区间
		// 上界（表未给单一值，取 128K/64K/32K 中较大的，注释记明依据）。
		"deepseek-v4-flash": {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		"glm-5.3-flash":     {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"hy3-preview-agent": {ContextWindow: 196608, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"hy3-preview":       {ContextWindow: 196608, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"hy4-preview":       {ContextWindow: 1048576, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"step-5-preview":    {ContextWindow: 1048576, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
	},
	"intl": {
		"deepseek-v4.1-flash": {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
		"hy4-preview":         {ContextWindow: 1048576, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"hy3-preview":         {ContextWindow: 196608, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"step-5-preview":      {ContextWindow: 1048576, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"kimi-k2.6":           {ContextWindow: 262144, MaxInput: 32768, Efforts: []string{"low", "medium", "high", "max"}},
		"glm-5.2":             {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"glm-5.1":             {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"glm-5.3":             {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"glm-5.3-flash":       {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"kimi-k3":             {ContextWindow: 1048576, MaxInput: 32768, Efforts: []string{"low", "medium", "high", "max"}},
		"kimi-k2.8-preview":   {ContextWindow: 1048576, MaxInput: 32768, Efforts: []string{"low", "medium", "high", "max"}},
		"minimax-m3":          {ContextWindow: 1048576, MaxInput: 32768, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-5.4":             {ContextWindow: 1048576, MaxInput: 32768, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-5.5":             {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-5.6-luna":        {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-5.6-terra":       {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-5.6-sol":         {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-6-astra":         {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-6-luna":          {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-6-sol":           {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gpt-6.1-sol":         {ContextWindow: 1048576, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"grok-4.7":            {ContextWindow: 500000, MaxInput: 131072, Efforts: []string{"low", "medium", "high", "max"}},
		"gemini-3.8-flash":    {ContextWindow: 1048576, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
		"gemini-3.5-flash":    {ContextWindow: 1048576, MaxInput: 65536, Efforts: []string{"low", "medium", "high", "max"}},
	},
}

// parseCtxWindow "1m"/"200k"/"1048576" → tokens；解析失败返回 0。
func parseCtxWindow(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	mult := 1
	num := s
	switch {
	case strings.HasSuffix(s, "m"):
		mult, num = 1000000, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, num = 1000, strings.TrimSuffix(s, "k")
	}
	v, err := strconv.Atoi(num)
	if err != nil {
		return 0
	}
	return v * mult
}

// (h *Hub) modelMeta 查模型元数据；ok=false 表示无可靠数据（不展示）。
func (h *Hub) modelMeta(line, model string) (modelMeta, bool) {
	switch line {
	case "qoder":
		p := h.qoderForAgg()
		if p == nil {
			return modelMeta{}, false
		}
		for _, e := range p.catalogSnapshot() {
			if e.Key == model || e.DisplayName == model {
				m := modelMeta{MaxInput: e.MaxInputTokens, Efforts: e.Efforts}
				for _, cw := range e.ContextWindows {
					if v := parseCtxWindow(cw); v > m.ContextWindow {
						m.ContextWindow = v
					}
				}
				return m, m.ContextWindow > 0 || m.MaxInput > 0 || len(m.Efforts) > 0
			}
		}
	case "zcode":
		p := h.zcodeForAgg()
		if p == nil {
			return modelMeta{}, false
		}
		for _, e := range p.catalogSnapshot() {
			if e.Key == model || e.DisplayName == model {
				m := modelMeta{MaxInput: e.MaxInputTokens, Efforts: e.Efforts}
				for _, cw := range e.ContextWindows {
					if v := parseCtxWindow(cw); v > m.ContextWindow {
						m.ContextWindow = v
					}
				}
				return m, m.ContextWindow > 0 || m.MaxInput > 0 || len(m.Efforts) > 0
			}
		}
	}
	// OpenCode 免费档（2026-10-15）：元数据是静态表（上游 /v1/models 不返回 limit），
	// 直接查本线路自己的模型清单。
	if line == "oc" {
		if m, ok := opencodeModelIDs[model]; ok {
			return modelMeta{ContextWindow: m.Context, MaxInput: m.Output, Efforts: m.Efforts}, true
		}
	}
	static, hasStatic := staticModelMeta[line][model]
	// 上游 /v3/config 目录优先给上下文：手工表会漏上新模型（2026-10-16 用户报
	// 「模型没有上下文」正是这批）。但目录不带思考档位，Efforts 仍用手工表的，
	// 两边合并而不是整条替换，免得有了上下文丢了档位。
	if line == "dom" || line == "intl" {
		if e, ok := wbCatalogLookup(line, model); ok && e.Context > 0 {
			m := static
			m.ContextWindow = e.Context
			// e.MaxInput 是**输出**上限（历史命名，见 wbCatalogEntry 的注释）。
			// 2026-10-19 前它被错填成上游的 maxInputTokens（输入上限 1000000），
			// 于是聚合网关把 max_output_tokens 报成 1000000 —— 客户端会以为能吐
			// 100 万 token。现在 parseWBConfigBody 改填 maxOutputTokens（131072）。
			if e.MaxInput > 0 {
				m.MaxInput = e.MaxInput
			}
			return m, true
		}
	}
	if hasStatic {
		return static, true
	}
	return modelMeta{}, false
}

// metaJSON 网关 API 输出形态。
func metaJSON(m modelMeta, ok bool) map[string]any {
	if !ok {
		return nil
	}
	out := map[string]any{}
	if m.ContextWindow > 0 {
		out["ctx"] = m.ContextWindow
	}
	if m.MaxInput > 0 {
		out["max_input"] = m.MaxInput
	}
	if len(m.Efforts) > 0 {
		out["efforts"] = m.Efforts
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
