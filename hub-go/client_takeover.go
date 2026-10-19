package main

// 客户端接管（2026-10-13 方案 C）：把 Codex / Claude Code 的配置从
// Hub Switch（:15721）切到 hub-go 统一网关（:8790）。原文件先备份。
//
// Codex（~/.codex/config.toml）：只动三处顶层键——
//   model_provider = "aihub"、model = 聚合 slug、注释掉 model_catalog_json；
//   [model_providers.aihub]（base_url=http://127.0.0.1:8790/v1, wire_api=responses）
//   用户配置里已存在，缺了就补一段。
// Claude Code（~/.claude/settings.json）：env 里 ANTHROPIC_BASE_URL/:8790、
//   AUTH_TOKEN=hub-local、MODEL 与四档别名指向聚合 slug；顶层 modelPicker
//   保留（其 ccs-claude-* 模型名 resolveAggModel 兼容）。
//
// 回退：备份文件（*.bak-hubgo-<时间戳>）还原即可，或把 Hub Switch 拉起来
// 点一下对应应用的供应商行（它会重写客户端配置）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	aggGatewayBase  = "http://127.0.0.1:8790"
	aggDefaultModel = "intl.deepseek-v4.1-flash"
)

func homeJoin(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}

func backupFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err // 文件不存在就没有备份一说
	}
	bak := fmt.Sprintf("%s.bak-hubgo-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(bak, b, 0644); err != nil {
		return "", err
	}
	return bak, nil
}

// dropStaleRouterSections 删掉 cc-switch/Hub Switch 时代写进 config.toml 的
// 死 provider 段。
//
// 2026-10-15 新增：方案 C 后 Codex 顶层 model_provider 指向 aihub（:8790），
// 但 [model_providers.custom]（base_url=http://127.0.0.1:15721/v1，
// experimental_bearer_token="PROXY_MANAGED"）仍留在文件里。它不生效，却会在
// 排障时误导（"这里怎么还写着 15721"）。接管时顺手清掉。
//
// 只删同时满足两个条件的段，避免误伤：
//  1. 段名形如 [model_providers.*]；
//  2. 段内 base_url 指向 127.0.0.1:15721（cc-switch 本地路由端口）。
func dropStaleRouterSections(lines []string) []string {
	const stalePort = "127.0.0.1:15721"
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		ln := lines[i]
		t := strings.TrimSpace(ln)
		// 找 [model_providers.xxx] 段的结束位置（下一个段头或文件尾）
		if strings.HasPrefix(t, "[model_providers.") {
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
				j++
			}
			body := strings.Join(lines[i+1:j], "\n")
			if strings.Contains(body, stalePort) {
				// 删整个段，连同段前的一个空行一起收拾掉
				for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
					out = out[:len(out)-1]
				}
				i = j
				// 段被删后若紧接另一段，补一个空行保持可读性
				if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
					out = append(out, "")
				}
				continue
			}
		}
		out = append(out, ln)
		i++
	}
	return out
}

// takeOverCodex 文本手术：改顶层 model_provider / model / model_catalog_json，
// 确保 [model_providers.aihub] 段存在。
//
// 2026-10-13 修复一个长期 bug：providerSet 曾被两个语义复用（顶层 provider 行、
// aihub 段是否存在），aihub 段已存在时顶层 model_provider 行**永远不会写入**，
// 桌面版就一直走官方 provider——选择器只剩官方推荐模型集。现在拆开：
//   - 顶层已有 model_provider 行 → 原地改值；
//   - 没有 → 在顶层区插入一行；
//   - model_catalog_json 不再注释掉：桌面版靠它拿模型列表（app-server 只在启动
//     读一次，不会拉网关 /v1/models），指向 hub-go 生成的 aihub-model-catalog.json
//     （见 codex_catalog.go），官方模型与第三方模型在同一份列表里共存。
func takeOverCodex() (string, error) {
	path, err := homeJoin(".codex", "config.toml")
	if err != nil {
		return "", err
	}
	catalogPath, err := homeJoin(".codex", codexCatalogPath)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	if bak, err := backupFile(path); err == nil {
		defer func() { _ = bak }() // 备份路径放返回信息里
	}
	lines := strings.Split(string(raw), "\n")
	lines = dropStaleRouterSections(lines)
	hasProviderLine, modelSet := false, false
	// 先扫一遍看 [model_providers.aihub] 段是否已存在（决定要不要补段）
	hasAihubSection := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "[model_providers.aihub]" {
			hasAihubSection = true
			break
		}
	}
	// 逐行替换（顶层第一处命中后不再动其它 section 里的同名键）
	topSection := true
	out := make([]string, 0, len(lines)+4)
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			if topSection && !hasProviderLine {
				// 顶层区结束还没见过 model_provider 行：在这里插入（provider + 目录指针）
				out = append(out, `model_provider = "aihub"`)
				out = append(out, `model_catalog_json = "`+catalogPath+`"`)
			}
			topSection = false
			out = append(out, ln)
			continue
		}
		if !topSection {
			out = append(out, ln)
			continue
		}
		if strings.HasPrefix(t, "model_provider") {
			out = append(out, `model_provider = "aihub"`)
			hasProviderLine = true
			continue
		}
		if strings.HasPrefix(t, "model_catalog_json") {
			out = append(out, `model_catalog_json = "`+catalogPath+`"`)
			continue
		}
		if !modelSet && strings.HasPrefix(t, "model") && strings.Contains(t, "=") &&
			!strings.HasPrefix(t, "model_provider") && !strings.HasPrefix(t, "model_reasoning") &&
			!strings.HasPrefix(t, "model_catalog_json") {
			out = append(out, `model = "`+aggDefaultModel+`"`)
			modelSet = true
			continue
		}
		out = append(out, ln)
	}
	if topSection && !hasProviderLine {
		// 整个文件没有 section（理论不会）：补在末尾
		out = append(out, `model_provider = "aihub"`)
		out = append(out, `model_catalog_json = "`+catalogPath+`"`)
	}
	var aihubSection []string
	if !hasAihubSection {
		aihubSection = []string{
			"",
			`[model_providers.aihub]`,
			`name = "AIHub 统一网关"`,
			`base_url = "` + aggGatewayBase + `/v1"`,
			`wire_api = "responses"`,
			`requires_openai_auth = true`,
		}
	}
	res := strings.Join(out, "\n")
	if len(aihubSection) > 0 {
		res += "\n" + strings.Join(aihubSection, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(res), 0644); err != nil {
		return "", err
	}
	// 目录文件：混合列表（第三方 + 官方），桌面版启动时读一次
	catalogMsg := ""
	if p, err := writeCodexCatalogFile(hubShared()); err == nil {
		catalogMsg = "；目录文件 " + p
	}
	msg := "Codex 已切到统一网关 :8790（model_provider=aihub，model=" + aggDefaultModel + catalogMsg + "；重开 Codex 生效）"
	return msg, nil
}

// hubShared 接管函数（包级函数，非 Hub 方法）里用的全局 Hub 引用：
// main 里赋值一次，仅供目录生成查快照。
var sharedHub *Hub

func hubShared() *Hub { return sharedHub }

// takeOverClaude settings.json 的 env 手术。
func takeOverClaude() (string, error) {
	path, err := homeJoin(".claude", "settings.json")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	if _, err := backupFile(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("settings.json 解析失败：%v", err)
	}
	env, _ := doc["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
		doc["env"] = env
	}
	env["ANTHROPIC_BASE_URL"] = aggGatewayBase
	env["ANTHROPIC_AUTH_TOKEN"] = "hub-local"
	env["ANTHROPIC_MODEL"] = aggDefaultModel
	for _, k := range []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		env[k] = aggDefaultModel
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0644); err != nil {
		return "", err
	}
	return "Claude Code 已切到统一网关 :8790（模型 " + aggDefaultModel + "；重开 Claude Code 生效）", nil
}

// restoreCodex 恢复 Codex 官方直连（与接管互逆）：
//   - 删顶层 model_provider = "aihub"（回到内置 openai provider + 官方登录）；
//   - 删顶层 model = 聚合 slug 行（intl./dom./qoder./trae./zcode. 或 gpt- 由网关
//     提供的模型名），Codex 回官方默认模型选择；
//   - [model_providers.aihub] 与 [profiles.*] 保留：无害，且用户还能用
//     codex --profile aihub 随时切回第三方。
//
// 官方登录（auth.json）全程不动，恢复后模型选择器就是官方推荐模型集。
func restoreCodex() (string, error) {
	path, err := homeJoin(".codex", "config.toml")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	if _, err := backupFile(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines))
	topSection := true
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			topSection = true // 后面还有顶层键？没有——TOML 里 [ 后全是 section；保守起见 section 内一律不动
		}
		if topSection && strings.HasPrefix(t, "model_provider") {
			continue // 删掉 provider 指向
		}
		if topSection && strings.HasPrefix(t, "model_catalog_json") {
			continue // 删掉桌面版目录指针，回官方推荐模型集
		}
		if topSection && strings.HasPrefix(t, "model") && strings.Contains(t, "=") &&
			!strings.HasPrefix(t, "model_provider") && !strings.HasPrefix(t, "model_reasoning") &&
			!strings.HasPrefix(t, "model_catalog_json") {
			// 顶层 model 若指向聚合 slug / 官方透传名，删除让 Codex 用官方默认
			if strings.Contains(t, `"intl.`) || strings.Contains(t, `"dom.`) ||
				strings.Contains(t, `"qoder.`) || strings.Contains(t, `"trae.`) ||
				strings.Contains(t, `"zcode.`) || strings.Contains(t, `"gpt-`) {
				continue
			}
		}
		out = append(out, ln)
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644); err != nil {
		return "", err
	}
	return "Codex 已恢复官方直连（登录身份保留；重开 Codex 生效）。[profiles.aihub] 仍在，可随时 codex --profile aihub 切回第三方", nil
}

// restoreClaude 恢复 Claude Code 官方直连：只删指向 :8790 网关的 env 键
// （值是 hub-local / :8790 / 聚合 slug 才删），用户自配的 DeepSeek 等键不动。
func restoreClaude() (string, error) {
	path, err := homeJoin(".claude", "settings.json")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	if _, err := backupFile(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("settings.json 解析失败：%v", err)
	}
	env, _ := doc["env"].(map[string]any)
	removed := 0
	if env != nil {
		for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
			v, ok := env[k].(string)
			if !ok {
				continue
			}
			if strings.Contains(v, "127.0.0.1:8790") || v == "hub-local" ||
				strings.HasPrefix(v, "intl.") || strings.HasPrefix(v, "dom.") ||
				strings.HasPrefix(v, "qoder.") || strings.HasPrefix(v, "trae.") ||
				strings.HasPrefix(v, "zcode.") {
				delete(env, k)
				removed++
			}
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Claude Code 已恢复官方直连（删除了 %d 个网关 env 键；重开 Claude Code 生效）", removed), nil
}

// apiClientsRestore POST /api/clients/restore {"app":"codex"|"claude"}
func (h *Hub) apiClientsRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		App string `json:"app"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var msg string
	var err error
	switch strings.TrimSpace(req.App) {
	case "codex":
		msg, err = restoreCodex()
	case "claude":
		msg, err = restoreClaude()
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "app 只支持 codex / claude"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logHub("客户端恢复官方：%s", msg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": msg})
}

// apiClientsTakeover POST /api/clients/takeover {"app":"codex"|"claude"}
func (h *Hub) apiClientsTakeover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		App string `json:"app"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var msg string
	var err error
	switch strings.TrimSpace(req.App) {
	case "codex":
		msg, err = takeOverCodex()
	case "claude":
		msg, err = takeOverClaude()
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "app 只支持 codex / claude"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	logHub("客户端接管：%s", msg)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": msg})
}

// apiRatios 倍率覆盖表：GET 列出（用户覆盖 + 静态表合并视图）；POST 保存覆盖。
// POST body: {"ratios":{"dom":{"deepseek-v4-flash":0.06,...},"intl":{...}}}
// 2026-10-13：各平台促销频繁变动，倍率以用户在客户端看到的实际显示为准，
// 控制台提供编辑入口（用户指正：不能瞎编，让我来维护）。
func (h *Hub) apiRatios(w http.ResponseWriter, r *http.Request) {
	loadUserRatios()
	if r.Method == http.MethodPost {
		var req struct {
			Ratios map[string]map[string]float64 `json:"ratios"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "bad json: " + err.Error()})
			return
		}
		userRatios = req.Ratios
		if err := saveUserRatios(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "保存失败：" + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "倍率已保存（即时生效）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": userRatios})
}
func (h *Hub) apiClientsStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true}
	if p, err := homeJoin(".codex", "config.toml"); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			re := regexp.MustCompile(`model_provider\s*=\s*"([^"]+)"`)
			m := re.FindStringSubmatch(string(b))
			out["codex_provider"] = m[1]
		}
	}
	if p, err := homeJoin(".claude", "settings.json"); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			var doc struct {
				Env struct {
					BaseURL string `json:"ANTHROPIC_BASE_URL"`
				} `json:"env"`
			}
			if json.Unmarshal(b, &doc) == nil {
				out["claude_base_url"] = doc.Env.BaseURL
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
