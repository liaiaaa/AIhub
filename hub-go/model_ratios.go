package main

// 模型计费倍率（0.xx，1 = 基准价；0 = 免费/限免）。
// 来源（2026-10 网上公开资料，各平台会调整，控制台仅作参考显示）：
//   - WorkBuddy 国内（copilot.tencent.com）：太空兔限免 0.03x、DS-V4-Flash 0.06x、
//     DS-V4-Pro 0.16x、MiniMax-M3 0.25x、Kimi-K2.6 0.52x、Kimi-K2.7 0.57x、
//     GLM-5.1/5.2 0.79x、GLM-5v-Turbo 0.95x、Hy3 0x
//   - WorkBuddy 国际（workbuddy.ai）：Hy3/Hy4 preview、DS-V4.1-Flash 免费，
//     GPT-5.6-Luna 0.14x、GPT-6-Astra 6.67x
//   - Qoder CN：官方模型目录自带 price_factor（qoder_models.go），不在此维护
//   - Trae / MiMo：没有公开的每模型倍率（Trae 按次扣积分），不显示
// 表里没有的模型不带倍率标注。

import "strconv"

var modelRatios = map[string]map[string]float64{
	"dom": {
		"space-bunny":       0.03,
		"deepseek-v4-flash": 0.06,
		"deepseek-v4-pro":   0.16,
		"minimax-m3-pay":    0.25,
		"kimi-k2.6":         0.52,
		"kimi-k2.7":         0.57,
		"glm-5.1":           0.79,
		"glm-5.2":           0.79,
		"glm-5v-turbo":      0.95,
		"hy3-preview-agent": 0,
	},
	"intl": {
		"deepseek-v4.1-flash": 0,
		"hy4-preview":         0,
		"gpt-5.6-luna":        0.14,
		"gpt-6-astra":         6.67,
	},
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
func ratioName(line, model string) string {
	r, ok := staticModelRatio(line, model)
	if !ok {
		return ""
	}
	return model + " · " + ratioLabel(r)
}

// (h *Hub) modelRatio 查某条线路上某模型的倍率；ok=false 表示无公开倍率数据。
// qoder 走官方目录的 price_factor（动态刷新跟随官方调整）。
func (h *Hub) modelRatio(line, model string) (float64, bool) {
	if line == "qoder" {
		p := h.qoderForAgg()
		if p == nil {
			return 0, false
		}
		for _, m := range p.catalogSnapshot() {
			if m.Key == model || m.DisplayName == model {
				if m.IsFree {
					return 0, true
				}
				return m.PriceFactor, true
			}
		}
		return 0, false
	}
	return staticModelRatio(line, model)
}
