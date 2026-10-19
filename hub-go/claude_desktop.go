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

import (
	"net/http"
	"strings"
)

// desktopRoleModels Desktop 安全角色名 → 聚合 slug。
// 日期后缀（claude-haiku-4-5-20251001 之类）按前缀匹配。
var desktopRoleModels = []struct {
	role  string
	slug  string
	label string
}{
	{"claude-sonnet-5", "intl.deepseek-v4.1-flash", "DeepSeek V4.1 Flash（国际）"},
	{"claude-sonnet-4-6", "intl.deepseek-v4.1-flash", "DeepSeek V4.1 Flash（国际）"},
	{"claude-opus-5", "dom.glm-5.3-flash", "GLM 5.3 Flash（国内）"},
	{"claude-opus-4-8", "dom.glm-5.3-flash", "GLM 5.3 Flash（国内）"},
	{"claude-fable-5", "qoder.Qwen3.8-Flash", "Qwen3.8 Flash（Qoder）"},
	{"claude-haiku-4-5", "trae.glm-5.2", "GLM 5.2（Trae）"},
}

// resolveDesktopRole 把 Desktop 角色名（含日期后缀）映射为聚合 slug；不是角色名返回空。
func resolveDesktopRole(model string) string {
	m := strings.TrimSpace(model)
	for _, r := range desktopRoleModels {
		if m == r.role || strings.HasPrefix(m, r.role+"-2") { // -20251001 日期后缀
			return r.slug
		}
	}
	return ""
}

// aggOpenAIModelsHandler /claude-desktop/v1/models：Desktop 的模型目录
// （角色名列表，与 profile 的 inferenceModels 一致）。
func (h *Hub) aggOpenAIModelsHandler(w http.ResponseWriter, r *http.Request) {
	data := make([]any, 0, len(desktopRoleModels))
	seen := map[string]bool{}
	for _, rm := range desktopRoleModels {
		if seen[rm.role] {
			continue
		}
		seen[rm.role] = true
		data = append(data, map[string]any{"id": rm.role, "object": "model", "supports1m": true})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
