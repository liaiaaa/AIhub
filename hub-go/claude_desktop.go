package main

// Claude Desktop 3P 接入（2026-10-13 方案 C 收尾）。
//
// 形状照抄 Hub Switch 的 claude_desktop_config.rs（写入目标、profile 字段、
// _meta.json 登记方式一致），这样 Desktop 看到的接入结果与 Hub Switch 时代
// 完全同构，只是网关从 :15721 换成 hub-go 统一网关 :8790：
//
//   ~/.config/Claude/claude_desktop_config.json      deploymentMode="3p"
//   ~/.config/Claude-3p/claude_desktop_config.json   deploymentMode="3p"（同上）
//   ~/.config/Claude-3p/configLibrary/<PROFILE_ID>.json   网关 profile：
//     { inferenceGatewayBaseUrl: "http://127.0.0.1:8790/claude-desktop",
//       inferenceGatewayAuthScheme: "bearer", inferenceGatewayApiKey: "hub-local",
//       inferenceProvider: "gateway", coworkEgressAllowedHosts: ["*"],
//       disableDeploymentModeChooser: true,
//       inferenceModels: [角色名 → Desktop 模型菜单] }
//   ~/.config/Claude-3p/configLibrary/_meta.json     entries 登记 + appliedId
//
// 模型名映射：Desktop 只会请求"安全角色名"（claude-sonnet-5 等，可能带日期
// 后缀），hub-go 的 resolveAggModel 里加了 desktop 角色表映射到聚合 slug。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	desktopProfileID   = "00000000-0000-4000-8000-000000157210"
	desktopProfileName = "AIHub 统一网关"
	desktopGatewayBase = "http://127.0.0.1:8790/claude-desktop"
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

// apiClaudeDesktopTakeover POST /api/claude_desktop/takeover
// 把 HubGo 网关 profile 写进 Claude Desktop（形状与 Hub Switch 一致）。
func (h *Hub) apiClaudeDesktopTakeover(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	configDir := filepath.Join(home, ".config")
	normalDir := filepath.Join(configDir, "Claude")
	threepDir := filepath.Join(configDir, "Claude-3p")
	libDir := filepath.Join(threepDir, "configLibrary")
	if err := os.MkdirAll(libDir, 0755); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}

	// 1) 两份 claude_desktop_config.json：deploymentMode="3p"（保留其余字段）
	for _, dir := range []string{normalDir, threepDir} {
		p := filepath.Join(dir, "claude_desktop_config.json")
		if err := patchJSONFile(p, func(doc map[string]any) {
			doc["deploymentMode"] = "3p"
		}); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": p + ": " + err.Error()})
			return
		}
	}

	// 2) 网关 profile（Desktop 模型菜单 = 角色名列表）
	models := make([]any, 0, len(desktopRoleModels))
	seen := map[string]bool{}
	for _, r := range desktopRoleModels {
		if seen[r.role] {
			continue
		}
		seen[r.role] = true
		item := map[string]any{"name": r.role, "labelOverride": r.label}
		item["supports1m"] = true
		models = append(models, item)
	}
	profile := map[string]any{
		"coworkEgressAllowedHosts":    []string{"*"},
		"disableDeploymentModeChooser": true,
		"inferenceGatewayApiKey":      "hub-local",
		"inferenceGatewayAuthScheme":  "bearer",
		"inferenceGatewayBaseUrl":     desktopGatewayBase,
		"inferenceProvider":           "gateway",
		"inferenceModels":             models,
	}
	profilePath := filepath.Join(libDir, desktopProfileID+".json")
	if err := writeJSONFile(profilePath, profile); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}

	// 3) _meta.json：登记 profile 并 appliedId 指向它
	metaPath := filepath.Join(libDir, "_meta.json")
	if err := patchJSONFile(metaPath, func(doc map[string]any) {
		entries, _ := doc["entries"].([]any)
		found := false
		for _, e := range entries {
			if em, ok := e.(map[string]any); ok && em["id"] == desktopProfileID {
				em["name"] = desktopProfileName
				found = true
			}
		}
		if !found {
			doc["entries"] = append(entries, map[string]any{"id": desktopProfileID, "name": desktopProfileName})
		}
		doc["appliedId"] = desktopProfileID
	}); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}

	logHub("Claude Desktop 已接入统一网关（profile %s）", desktopProfileID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true,
		"msg": "Claude Desktop 已接入 :8790（profile 已写入，重启 Claude Desktop 生效）"})
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

// patchJSONFile 读改写一个 JSON 对象文件；文件不存在则新建。
func patchJSONFile(path string, patch func(map[string]any)) error {
	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return fmt.Errorf("解析失败: %v", err)
		}
	}
	patch(doc)
	return writeJSONFile(path, doc)
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
