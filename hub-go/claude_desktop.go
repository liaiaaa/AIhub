package main

// Claude Desktop 角色名映射（:8790 的 /claude-desktop 协议入口）。
//
// 2026-10-19 用户要求「去掉 Claude 客户端接管，只留 Codex 的」：
//   - 已删除 POST /api/claude_desktop/takeover 与它写入的 3P profile
//     （desktopProfileID / desktopProfileName / desktopGatewayBase 一并移除）；
//   - Claude Code 与 Claude Desktop 已恢复官方直连；
//   - 但 :8790 仍提供 /claude-desktop/v1/messages 与 /claude-desktop/v1/models
//     两个协议入口（手填 base_url 仍可用），因此角色名映射表保留。
//
// 模型名映射：Desktop 只会请求"安全角色名"（claude-sonnet-5 等，可能带日期
// 后缀），hub-go 的 resolveAggModel 里用下面这张表映射到聚合 slug。
//
// 2026-11-09 用户报「intl 线路不咋用，能不能只用聚合口的模型」：原表把
// claude-sonnet-* 写死到 intl.deepseek-v4.1-flash，intl 一停整个 Code 模式就
// 503。改成「偏好 + 兜底」——优先用下面列的首选 slug，该 slug 不在当前聚合
// 目录里时按顺序取聚合目录里的下一个顶上，所以只要聚合口还有模型，Code 就
// 不会因为某条线路停摆而整体不可用。

import (
	"net/http"
	"strings"
)

// desktopRolePrefs Desktop 安全角色名 → 首选聚合 slug。
// 日期后缀（claude-haiku-4-5-20251001 之类）按前缀匹配。
// 首选 slug 不在当前聚合目录时按顺序兜底（见 desktopRoleTable）。
var desktopRolePrefs = []struct {
	role string
	slug string
}{
	{"claude-sonnet-5", "dom.deepseek-v4-flash"},
	{"claude-sonnet-4-6", "qoder.DeepSeek-Flash"},
	{"claude-opus-5", "qoder.Qwen3.8-Flash"},
	{"claude-opus-4-8", "qoder.QoDuck"},
	{"claude-fable-5", "teleagent.chat-pro"},
	{"claude-haiku-4-5", "teleagent.chat-flagship"},
}

// desktopRoleEntry 一条实际生效的映射。
type desktopRoleEntry struct {
	Role  string
	Slug  string
	Label string
}

// desktopRoleTable 按当前聚合目录算出实际生效的角色名映射。
//
// 规则：首选 slug 在聚合目录里就用它；不在就按位置取聚合目录里的下一个；
// 聚合目录为空时退回首选 slug（保持「不因目录空而整个入口失效」的旧行为）。
func desktopRoleTable() []desktopRoleEntry {
	snap := aggSnapshot()
	out := make([]desktopRoleEntry, 0, len(desktopRolePrefs))
	for i, p := range desktopRolePrefs {
		e := desktopRoleEntry{Role: p.role, Slug: p.slug}
		for _, am := range snap {
			if am.Slug == p.slug {
				e.Slug, e.Label = am.Slug, am.Display
				break
			}
		}
		if e.Label == "" && len(snap) > 0 {
			am := snap[i%len(snap)]
			e.Slug, e.Label = am.Slug, am.Display
		}
		if e.Label == "" {
			e.Label = e.Slug // 聚合目录为空：只回 slug，不编显示名
		}
		out = append(out, e)
	}
	return out
}

// resolveDesktopRole 把 Desktop 角色名（含日期后缀）映射为聚合 slug；不是角色名返回空。
func resolveDesktopRole(model string) string {
	m := strings.TrimSpace(model)
	for _, e := range desktopRoleTable() {
		if m == e.Role || strings.HasPrefix(m, e.Role+"-2") { // -20251001 日期后缀
			return e.Slug
		}
	}
	return ""
}

// aggOpenAIModelsHandler /claude-desktop/v1/models：Desktop 的模型目录
// （角色名列表，与 profile 的 inferenceModels 一致）。
func (h *Hub) aggOpenAIModelsHandler(w http.ResponseWriter, r *http.Request) {
	table := desktopRoleTable()
	data := make([]any, 0, len(table))
	seen := map[string]bool{}
	for _, e := range table {
		if seen[e.Role] {
			continue
		}
		seen[e.Role] = true
		data = append(data, map[string]any{
			"id": e.Role, "object": "model", "supports1m": true,
			"display_name": e.Label,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
