package main

// 统一思考档位（2026-11-03）。
//
// 问题：8 条线路的「思考档位」名字各不相同 —— trae 只认 light/high/extra_high，
// zcode 只认 low/high/max，qoder 每个模型是不同子集，dom/intl 吃 minimal…max；
// 而客户端（DSH / Codex / curl）看到的是同一份 /v1/models，需要一套统一说法。
//
// 这里定义**规范档位**（按强度升序）：
//
//	minimal < low < medium < high < xhigh < max
//
// 各线路对外只宣告「自己真支持的那几档」（经映射后的规范名），收到客户端的规范名
// 再翻回上游认的名字。这样客户端菜单不会出现上游不认的档位，也不会因为上游叫法
// 奇怪（extra_high）而被静默降档。
//
// 事实来源（逐条读码确认）：
//   - dom/intl：staticModelMeta[line][model].Efforts（model_meta.go）
//   - qoder    ：运行时官方目录 thinking_config → catalogSnapshot().Efforts
//   - zcode    ：zcode_models.go + 远端刷新目录的 Efforts
//   - trae     ：traeModelEfforts（trae_proxy.go，桌面端 vscdb 抄来，light/high/extra_high）
//   - oc       ：opencodeFreeModels[].Efforts（只有部分模型有）
//   - teleagent：上游网关认 reasoning_effort ∈ {low,high,max}（2026-11-03 实测：
//                 传 minimal 返回 400「该模型始终思考，不支持关闭思考；请使用 low、high 或 max」，
//                 传 low/high/max 均 200 且思考深度明显不同）
//   - mimo：本机拿不到 MiMo 登录数据，未能实测 → effortsOf 返回 false，暂不宣告

import "sort"

// canonicalEffortOrder 规范档位的强度顺序。
var canonicalEffortOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// lineEfforts 某条线路某个模型的档位信息。
type lineEfforts struct {
	Levels  []string // 规范档位，按强度升序，已去重
	Default string   // 客户端不指定时实际会用的档位（空=不注入）
}

// trae 官方档位 ↔ 规范档位。trae 只有三档，所以是把 6 档压到 3 档。
var (
	traeNativeToCanonical = map[string]string{
		"light":      "low",
		"high":       "high",
		"extra_high": "max",
	}
	traeCanonicalToNative = map[string]string{
		"minimal": "light",
		"low":     "light",
		"medium":  "high",
		"high":    "high",
		"xhigh":   "extra_high",
		"max":     "extra_high",
	}
	// teleagent 认 low/high/max；「不思考」不被接受（400），所以 minimal 折到 low
	teleagentCanonicalToNative = map[string]string{
		"minimal": "low",
		"low":     "low",
		"medium":  "high",
		"high":    "high",
		"xhigh":   "max",
		"max":     "max",
	}
	// zcode 只认 low/high/max
	zcodeCanonicalToNative = map[string]string{
		"minimal": "low",
		"low":     "low",
		"medium":  "high",
		"high":    "high",
		"xhigh":   "max",
		"max":     "max",
	}
)

// canonicalEffortRank 规范档位的强度排名；未知返回 -1。
func canonicalEffortRank(s string) int {
	for i, v := range canonicalEffortOrder {
		if v == s {
			return i
		}
	}
	return -1
}

// effortsOf 返回某线路某模型的档位（唯一事实源）。ok=false 表示上游没有档位配置，
// 调用方不应向客户端宣告档位。
func (h *Hub) effortsOf(line, model string) (lineEfforts, bool) {
	var native []string
	switch line {
	case "teleagent":
		// 实测支持的三档（上游不认「关闭思考」，所以没有 minimal）
		return lineEfforts{Levels: []string{"low", "high", "max"}, Default: "max"}, true
	case "trae":
		// trae 的档位不在 staticModelMeta 里，另有真值表（含 nil=不支持思考）。
		native = traeModelEfforts[model]
	default:
		if m, ok := h.modelMeta(line, model); ok {
			native = m.Efforts
		}
	}
	if len(native) == 0 {
		return lineEfforts{}, false
	}

	levels := canonicalizeEfforts(line, native)
	if len(levels) == 0 {
		return lineEfforts{}, false
	}
	def := ""
	if line != "oc" {
		// 除 oc 外，各线路「客户端不说档位」时都会注入该模型最高档
		// （dom/intl/mimo: max；qoder/zcode: 最高；trae: extra_high→max）。
		def = levels[len(levels)-1]
	}
	return lineEfforts{Levels: levels, Default: def}, true
}

// canonicalizeEfforts 把某线路的原生档位名转成规范名，去重并按强度升序。
// 认不出的名字直接丢掉（宁缺不编：宣告了上游不认的档位只会静默降档）。
func canonicalizeEfforts(line string, native []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(native))
	for _, n := range native {
		c := n
		switch line {
		case "trae":
			if v, ok := traeNativeToCanonical[n]; ok {
				c = v
			} else if canonicalEffortRank(n) >= 0 {
				c = n // 已经是规范名也接受
			} else {
				continue
			}
		case "zcode":
			// zcode 原生名就是规范名的子集（low/high/max）
			if canonicalEffortRank(n) < 0 {
				continue
			}
		default:
			if canonicalEffortRank(n) < 0 {
				continue
			}
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return canonicalEffortRank(out[i]) < canonicalEffortRank(out[j]) })
	return out
}

// canonicalToWireEffort 把客户端要的规范档位翻成该线路认的名字。
// ok=false 表示这条线路没有这个档位（调用方应钳制到最近的档，而不是原样透传）。
func canonicalToWireEffort(line, want string) (string, bool) {
	w := lowerTrim(want)
	if w == "" {
		return "", false
	}
	switch line {
	case "trae":
		if v, ok := traeCanonicalToNative[w]; ok {
			return v, true
		}
		// 客户端直接发原生名（light/high/extra_high）也放行
		if _, ok := traeNativeToCanonical[w]; ok {
			return w, true
		}
		return "", false
	case "teleagent":
		if v, ok := teleagentCanonicalToNative[w]; ok {
			return v, true
		}
		return "", false
	case "zcode":
		if v, ok := zcodeCanonicalToNative[w]; ok {
			return v, true
		}
		return "", false
	default:
		if canonicalEffortRank(w) < 0 {
			return "", false
		}
		return w, true
	}
}

// wireToCanonicalEffort 反过来：把上游档位名还原成规范名（日志/回显用）。
func wireToCanonicalEffort(line, wire string) string {
	w := lowerTrim(wire)
	if line == "trae" {
		if v, ok := traeNativeToCanonical[w]; ok {
			return v
		}
	}
	return w
}

// clampEffort 把不在该模型档位表里的规范档位钳到最近的一档。
// 用于「客户端发了一个这模型没有的档位」——就近钳制比拒绝更符合直觉
// （沿用 qoder_thinking.go 的既有做法）。
func clampEffort(levels []string, want string) string {
	if len(levels) == 0 {
		return ""
	}
	rank := canonicalEffortRank(want)
	if rank < 0 {
		return levels[len(levels)-1] // 认不出就取最高档
	}
	best := levels[0]
	bestDiff := 1 << 30
	for _, l := range levels {
		d := canonicalEffortRank(l) - rank
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			best, bestDiff = l, d
		}
	}
	return best
}

// lowerTrim 小写去空格（档位名大小写不敏感：客户端有的发 Max、有的发 max）。
func lowerTrim(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		out = append(out, r)
	}
	return string(out)
}
