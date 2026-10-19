package main

// 模型计费倍率（0.xx，1 = 基准价；0 = 免费/限免）。
// 来源分三层（优先级从高到低）：
//   1. 用户覆盖表 model_ratios_user.json（控制台可编辑——各平台促销频繁变动，
//      最可靠的维护人是用户自己，以客户端实际显示为准）
//   2. 静态表 modelRatios（2026-10 公开资料；intl 的"免费"标注已于 2026-10-13
//      撤下：当日实测 29M in/321K out 被扣 159.77 积分，证明"免费"有条件或已变化）
//   3. qoder 官方目录 price_factor（动态刷新，不在此维护）
// 表里没有的模型不带倍率标注（不瞎编）。

import (
	"encoding/json"
	"os"
	"strconv"
)

var modelRatios = map[string]map[string]float64{
	// 2026-10-13 依用户桌面端实拍矫正（倍率/workbuddy国内.md）：
	// Hy3 0.05x（限时免费档另计）、Step-5 0（独家限免）、Space-Bunny 0.08x、
	// DeepSeek-V4-Pro 0.51x、GLM-5.3 0.79x、GLM-5.3-Flash 0.06x（订阅优先）、
	// GLM-5.2 0.79x（夜间折扣）、Kimi-K3 1.62x、MiniMax-M3 0.25x。
	// intl 的 Hy4 preview 0.29x / DeepSeek-V4.1-Flash 0.11x 一并录入。
	"dom": {
		// 2026-10-15 依 倍率/workbuddy国内.md 补齐（此前只有部分模型）。
		// 注意：国内表里 Hy4 preview High 0.29x、Hy3 High 有「0.00x 限时免费 /
		// 0.05x 非免费」两行、Step-5-Preview Medium 0.00x 独家限免。
		"space-bunny":       0.08,
		"space-bunny-max":   0.08,
		"deepseek-v4-flash": 0.06,
		"deepseek-v4-pro":   0.51,
		"minimax-m3-pay":    0.25,
		"kimi-k2.6":         0.52,
		"kimi-k2.7":         0.57,
		"kimi-k3":           1.62,
		"glm-5.1":           0.79,
		"glm-5.2":           0.79,
		"glm-5.3":           0.79,
		"glm-5.3-flash":     0.06,
		"glm-5v-turbo":      0.95,
		"hy3-preview-agent": 0.05,
		"hy3-preview":       0.05,
		"hy4-preview":       0.29, // 国内表：Hy4 preview High 0.29x
		"step-5-preview":    0,   // 桌面端显示「独家限免 0.00x」
	},
	"intl": {
		// 2026-10-15 依 倍率/workbuddy国际.md 全量录入（20 个模型）。
		// Hy4 / Hy3 / Step-5 / DeepSeek-V4.1-Flash 桌面端显示 0.00x（Free now /
		// Exclusive Free）——注意 10-13 早间实测 deepseek-v4.1-flash 被扣过费，
		// "免费"可能有时段/额度条件，再遇扣费以实测为准。
		"hy4-preview":         0,
		"hy3-preview":         0,
		"step-5-preview":      0,
		"deepseek-v4.1-flash": 0,
		"deepseek-v4-pro":     0.51,
		"space-bunny-max":     0.08,
		// GPT / Grok / Gemini 系列（国际端目录，比官方订阅便宜）
		"gpt-6-astra":   6.67,
		"gpt-6.1-sol":   1.33,
		"gpt-6-sol":     1.33,
		"gpt-6-luna":    0.07,
		"gpt-5.6-sol":   3.47,
		"gpt-5.6-terra": 1.39,
		"gpt-5.6-luna":  0.14,
		"gpt-5.5":       3.31,
		"gpt-5.4":       1.65,
		"grok-4.7":      1.9,
		"gemini-3.8-flash": 0.95,
		"gemini-3.5-flash": 0.99,
		"glm-5.3":       0.79,
		"glm-5.3-flash": 0.06,
		"glm-5.2":       0.79,
		"glm-5.1":       0.79,
		"kimi-k3":       1.62,
		"kimi-k2.6":     0.52,
		"kimi-k2.8-preview": 0.77,
		"minimax-m3":    0.25,
	},
	// 2026-10-15 依 倍率/Qoder.md 录入（此前 qoder 走官方目录 price_factor，
	// 但官方目录缺「快速/Fast」等条目的倍率，静态表兜底）。
	// 注意：key 用客户端可见的 DisplayName（Qwen3.8-Flash），与 modelRatio 的
	// `m.Key == model || m.DisplayName == model` 匹配；官方目录的 Key 是内部 id
	// （qfmodel 等），只有线路启动时才查得到，所以静态表必须自备一份。
	// Qoder.md 记的是低峰折扣后的值，非折扣期可能不同。
	"qoder": {
		"Auto":              0.5,
		"Fast":              0.8,
		"Qwen3.8-Max":       0.5,
		"Qwen3.8-Flash":     0.1,
		"Qwen3.7-Max":       0.5,
		"Qwen3.7-Plus":      0.1,
		"Qwen3.7-Flash":     0.1,
		"DeepSeek-V4-Pro":   0.5,
		"DeepSeek-Flash":    0.1,
		"GLM-5.3":           0.8,
		"GLM-5.3-Flash":     0.1,
		"GLM-5.2":           0.6,
		"Kimi-K3":           1.4,
		"Kimi-K2.8-Preview": 0.8,
		"MiniMax-M2.7":      0.2,
	},
}

// userRatios 用户覆盖表（model_ratios_user.json），懒加载 + 保存时写盘。
var userRatios map[string]map[string]float64

func userRatiosPath() string { return dataPath("model_ratios_user.json") }

func loadUserRatios() {
	if userRatios != nil {
		return
	}
	userRatios = map[string]map[string]float64{}
	b, err := os.ReadFile(userRatiosPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &userRatios)
}

// saveUserRatiosLocked 写盘（调用方保证并发安全由 handler 串行化即可）。
func saveUserRatios() error {
	b, err := json.MarshalIndent(userRatios, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(userRatiosPath(), b, 0644)
}

// staticModelRatio 查静态倍率表（dom/intl 等写死线路）。
func staticModelRatio(line, model string) (float64, bool) {
	r, ok := modelRatios[line][model]
	return r, ok
}

// ratioLabel 倍率展示文案："免费" / "0.03x" / "6.67x"。
func ratioLabel(r float64) string {
	if r == 0 {
		return "免费"
	}
	return strconv.FormatFloat(r, 'f', -1, 64) + "x"
}

// ratioName 给 /v1/models 条目生成带倍率的显示名（dsh 等客户端读 name 字段，
// 选择器悬停提示与添加后的模型名都用它），无倍率数据时返回空串，
// 调用方就不写 name，客户端回退用 id。
//
// 数据源优先级：上游 /v3/config 目录 > 手工静态表 model_ratios.go。
// 2026-10-16 起优先读目录——倍率和上下文是上游按账号下发的，手工表跟不上上新
// （用户当天报「模型没有倍率和上下文」就是手工表漏了 glm-5.3、step-5-preview 这一批）。
// 上下文一并带上：DSH 的解析器不读自定义字段，只能并进 name 才看得见。
func ratioName(line, model string) string {
	if e, ok := wbCatalogLookup(line, model); ok && e.Ratio != "" {
		return withCtx(model, e.Ratio, e.Context)
	}
	r, ok := staticModelRatio(line, model)
	if !ok {
		return ""
	}
	ctx := 0
	if e, ok := wbCatalogLookup(line, model); ok {
		ctx = e.Context
	}
	return withCtx(model, ratioLabel(r), ctx)
}

// withCtx 拼显示名。上下文为 0 时不带这段（不硬编，宁缺）。
func withCtx(model, ratio string, ctx int) string {
	if ctx <= 0 {
		return model + " · " + ratio
	}
	return model + " · " + ratio + " · 上下文 " + humanTokens(ctx)
}

// humanTokens 1000000 → "1M"，256000 → "256K"。
func humanTokens(n int) string {
	switch {
	case n >= 1000000 && n%1000000 == 0:
		return strconv.Itoa(n/1000000) + "M"
	case n >= 1000:
		return strconv.Itoa(n/1000) + "K"
	}
	return strconv.Itoa(n)
}

// (h *Hub) modelRatio 查某条线路上某模型的倍率；ok=false 表示无倍率数据。
// 优先级：用户覆盖表 > qoder 官方目录 price_factor > 上游 /v3/config 目录 > 静态表。
//
// 2026-10-15 修复：qoder 线路没启动（qoderForAgg 返回 nil）或官方目录里没有
// 该条目时，原来直接 return 0,false——线路没启动就彻底没有倍率标注。现在统一
// 回落到静态表（倍率/Qoder.md 录入），目录不启动也能显示倍率。
//
// 2026-10-16 追加：dom/intl 先查上游 /v3/config 目录再落静态表。手工表会漏上新
// 模型，目录是账号级实时下发的，以它为准；抓不到时（线路没起、无凭据）照旧回落。
func (h *Hub) modelRatio(line, model string) (float64, bool) {
	// OpenCode 免费档（2026-10-15）：这条线只挂免费模型，倍率恒为 0（显示「免费」）。
	if line == "oc" {
		_, ok := opencodeModelIDs[model]
		return 0, ok
	}
	loadUserRatios()
	if m, ok := userRatios[line]; ok {
		if v, ok := m[model]; ok {
			return v, true
		}
	}
	if line == "dom" || line == "intl" {
		if v, ok := wbCatalogRatioValue(line, model); ok {
			return v, true
		}
	}
	if line == "qoder" {
		if p := h.qoderForAgg(); p != nil {
			for _, m := range p.catalogSnapshot() {
				if m.Key == model || m.DisplayName == model {
					if m.IsFree {
						return 0, true
					}
					return m.PriceFactor, true
				}
			}
		}
		// 官方目录没这项 / 线路未启动：回落静态表
		if r, ok := staticModelRatio(line, model); ok {
			return r, true
		}
		return 0, false
	}
	return staticModelRatio(line, model)
}
