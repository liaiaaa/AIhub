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
	"sort"
	"strings"
	"time"
)

const (
	aggGatewayBase  = "http://127.0.0.1:8790"
	aggDefaultModel = "intl.deepseek-v4.1-flash"
)

// A′ 的 Codex 原生 responses 前缀。实测 Codex 0.167 会把 /responses 拼在
// openai_base_url 后面，因此网关侧必须在这个前缀上提供 handler。
const codexBasePath = "/backend-api/codex"

// codexBaseURL 是写进 ~/.codex/config.toml 顶层 openai_base_url 的值。
const codexBaseURL = aggGatewayBase + codexBasePath

// userHomeDir 是 homeJoin 的 home 来源，做成变量以便测试注入。
//
// 2026-10-27 事故：TestCodexCatalogFileLocation 故意把白名单清空后调用真实
// 写盘路径，把用户真实的 ~/.codex/aihub-model-catalog.json 写成 {"models": []}，
// Codex 桌面版启动报 `must contain at least one model` 打不开，而且它只在启动时
// 读一次、不会自愈。测试里必须改成临时 home，不能再碰真实用户目录。
var userHomeDir = os.UserHomeDir

func homeJoin(elem ...string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}

// backupFileTagged 备份 path 为 <path>.bak-<tag>-<时间戳>，沿用原 mode（取不到用 0600）。
// tag 用来区分「接管前」与「还原前」两类备份：还原只认 bak-hubgo-*，
// 否则第二次还原会把上一轮的还原备份当成原始态，越还原越乱。
func backupFileTagged(path, tag string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err // 文件不存在就没有备份一说
	}
	bak := fmt.Sprintf("%s.bak-%s-%s", path, tag, time.Now().Format("20060102-150405"))
	// 2026-10-21 开源审查修：原来固定 0644。被备份的是 ~/.codex/config.toml
	// 这类文件（原权限可能是 0600，且常含其他供应商的 API key），备份留在
	// 家目录里长期不动，不该比原文件更宽松。沿用原 mode，取不到就 0600。
	mode := os.FileMode(0600)
	if st, serr := os.Stat(path); serr == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(bak, b, mode); err != nil {
		return "", err
	}
	return bak, nil
}

// backupFile 接管前备份（保持既有调用方语义与文件名）。
func backupFile(path string) (string, error) { return backupFileTagged(path, "hubgo") }

// latestBackup 返回 <path>.bak-<tag>-* 里最新的一个；没有返回空串。
func latestBackup(path, tag string) string {
	matches, err := filepath.Glob(fmt.Sprintf("%s.bak-%s-*", path, tag))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches) // 时间戳定长，字典序即时间序
	return matches[len(matches)-1]
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

// takeOverCodex A′ 接管（2026-11-09）：把 responses 经统一网关转发，
// 同时把「官方 + 第三方」混合目录写成 Codex 的选择器目录。
//
// 与旧实现（model_provider=aihub + /v1/responses）的区别，实测依据见
// docs/superpowers/specs/2026-11-09-codex-mixed-model-catalog-design.md：
//   - 顶层必须**移除** model_provider：留着它 responses 会走
//     [model_providers.aihub].base_url（/v1/responses），而不是 openai_base_url；
//   - 顶层必须**保留** model_catalog_json：Codex 0.167 的选择器只读它或内置目录，
//     不会来拉 /models（实测 models_hits=0）。
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
	bak, _ := backupFile(path)

	lines := strings.Split(string(raw), "\n")
	lines = dropStaleRouterSections(lines)

	hasAihubSection := false
	hasCatalogLine := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "[model_providers.aihub]" {
			hasAihubSection = true
		}
		if strings.HasPrefix(t, "model_catalog_json") {
			hasCatalogLine = true
		}
	}

	baseComment := "# aihub: Codex 的 responses 经统一网关转发（删除本行与下面 openai_base_url 即恢复官方直连）"
	baseLine := `openai_base_url = "` + codexBaseURL + `"`

	out := make([]string, 0, len(lines)+6)
	topSection := true
	baseWritten := false
	modelSet := false
	writeBase := func() {
		if baseWritten {
			return
		}
		out = append(out, baseComment, baseLine)
		baseWritten = true
	}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			if topSection {
				writeBase() // 顶层区结束前补上 openai_base_url
			}
			topSection = false
			out = append(out, ln)
			continue
		}
		if !topSection {
			out = append(out, ln)
			continue
		}
		// 顶层：删 model_provider（关键），改写 openai_base_url / model_catalog_json
		if strings.HasPrefix(t, "model_provider") {
			continue
		}
		if strings.HasPrefix(t, "openai_base_url") {
			writeBase()
			continue
		}
		if strings.HasPrefix(t, "model_catalog_json") {
			out = append(out, `model_catalog_json = "`+catalogPath+`"`)
			continue
		}
		if !modelSet && strings.HasPrefix(t, "model") && strings.Contains(t, "=") &&
			!strings.HasPrefix(t, "model_provider") && !strings.HasPrefix(t, "model_reasoning") &&
			!strings.HasPrefix(t, "model_catalog_json") && !strings.HasPrefix(t, "model_verbosity") {
			out = append(out, `model = "`+aggDefaultModel+`"`)
			modelSet = true
			continue
		}
		out = append(out, ln)
	}
	writeBase()
	if !hasCatalogLine {
		out = append(out, `model_catalog_json = "`+catalogPath+`"`)
	}
	res := strings.Join(out, "\n")
	if !hasAihubSection {
		res += "\n" + strings.Join([]string{
			"",
			`[model_providers.aihub]`,
			`name = "AIHub 统一网关"`,
			`base_url = "` + aggGatewayBase + `/v1"`,
			`wire_api = "responses"`,
			`requires_openai_auth = true`,
		}, "\n") + "\n"
	}
	// 原子写：先写 tmp 再 rename，避免写一半留下坏 TOML 让 Codex 起不来。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(res), 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	catalogMsg := ""
	if p, cerr := writeCodexMixedCatalog(hubShared()); cerr == nil {
		catalogMsg = "；混合目录 " + p
	} else {
		catalogMsg = "；⚠ 混合目录生成失败：" + cerr.Error()
	}
	msg := "Codex 已切到统一网关 :8790（openai_base_url=" + codexBaseURL + "，model=" + aggDefaultModel + catalogMsg
	if bak != "" {
		msg += "；原配置已备份为 " + filepath.Base(bak)
	}
	msg += "；重开 Codex 生效）"
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

// restoreCodex 恢复 Codex 官方直连。
//
// 2026-11-09（A′）：改为「从最新 bak-hubgo-* 逐字节还原」，这样接管期间被改写的
// model / model_catalog_json / 删掉的 model_provider 都能一字不差回来；
// 还原动作自己的备份用 bak-restore-* 后缀，避免下次还原把还原态当原始态。
// 没有 bak-hubgo-* 时退回文本手术（只删 aihub 加的键），保证老环境也能恢复。
func restoreCodex() (string, error) {
	path, err := homeJoin(".codex", "config.toml")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%v", path, err)
	}
	if _, err := backupFileTagged(path, "restore"); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if bak := latestBackup(path, "hubgo"); bak != "" {
		bb, rerr := os.ReadFile(bak)
		if rerr != nil {
			return "", fmt.Errorf("读备份 %s 失败：%v", bak, rerr)
		}
		tmp := path + ".tmp"
		if werr := os.WriteFile(tmp, bb, 0644); werr != nil {
			return "", werr
		}
		if rerr := os.Rename(tmp, path); rerr != nil {
			_ = os.Remove(tmp)
			return "", rerr
		}
		return "Codex 已恢复官方直连（从备份 " + filepath.Base(bak) + " 逐字节还原；重开 Codex 生效）", nil
	}

	// 兜底：没有备份时做文本手术，只删 aihub 加的键。
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines))
	topSection := true
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			topSection = false
			out = append(out, ln)
			continue
		}
		if topSection {
			if strings.HasPrefix(t, "openai_base_url") || strings.HasPrefix(t, "model_provider") ||
				strings.HasPrefix(t, "model_catalog_json") || strings.HasPrefix(t, "# aihub:") {
				continue
			}
			if strings.HasPrefix(t, "model") && strings.Contains(t, "=") &&
				!strings.HasPrefix(t, "model_reasoning") {
				if strings.Contains(t, `"intl.`) || strings.Contains(t, `"dom.`) ||
					strings.Contains(t, `"qoder.`) || strings.Contains(t, `"trae.`) ||
					strings.Contains(t, `"zcode.`) || strings.Contains(t, `"teleagent.`) ||
					strings.Contains(t, `"oc.`) {
					continue
				}
			}
		}
		out = append(out, ln)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")), 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return "Codex 已恢复官方直连（无备份可还原，已删除网关相关键；重开 Codex 生效）", nil
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
	case "dsh":
		msg, err = restoreDSHLatest()
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

// apiClientsTakeover POST /api/clients/takeover {"app":"codex"|"claude"|"dsh"}
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
	case "dsh":
		msg, err = takeOverDSH(h)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "app 只支持 codex / claude / dsh"})
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
		userRatiosReplace(req.Ratios)
		if err := saveUserRatios(); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "保存失败：" + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "倍率已保存（即时生效）"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": userRatiosSnapshot()})
}

// apiClientsStatus 客户端接管状态。
// 2026-11-09（A′）：口径从 model_provider 改为 openai_base_url；
// 同时修掉旧实现 m[1] 无匹配时的越界 panic。
func (h *Hub) apiClientsStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"ok": true}
	if p, err := homeJoin(".codex", "config.toml"); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			s := string(b)
			baseURL := ""
			if m := regexp.MustCompile(`(?m)^\s*openai_base_url\s*=\s*"([^"]*)"`).FindStringSubmatch(s); len(m) > 1 {
				baseURL = m[1]
			}
			provider := ""
			if m := regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]*)"`).FindStringSubmatch(s); len(m) > 1 {
				provider = m[1]
			}
			out["codex_base_url"] = baseURL
			out["codex_provider"] = provider // 保留旧字段，前端还有兼容分支
			out["codex_taken_over"] = strings.Contains(baseURL, "/backend-api/codex")
		}
	}
	// 2026-10-19：Claude Code / Claude Desktop 已恢复官方直连，控制台不再接管它们，
	// 所以这里不再回报 claude_base_url（前端也已去掉对应显示）。
	// 2026-11-03：加上 DSH 接管状态（是否已配 aihub 模型、备份份数）。
	out["dsh"] = dshTakeoverStatus()
	writeJSON(w, http.StatusOK, out)
}
