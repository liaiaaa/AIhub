package main

// DSH（DeepSeek Harness）接管：把 aihub 的模型清单写进 DSH 的 profile。
//
// 背景（2026-11-03，用户问「为什么 magpie 能直接填写好思考等级」）：
// DSH 的模型发现只读 4 个字段（id / name / contextWindow / maxTokens），**不读档位**；
// 它给内置厂商（deepseek/openai/…）自动显示档位，是因为 pi-ai 自带目录
// （`@earendil-works/pi-ai/dist/providers/data/<provider>.json`）里写着
// `reasoning: true` + `thinkingLevelMap`。接自定义 baseURL（aihub）时没有这份目录，
// 档位只能来自 profile 的 `models[].reasoningEfforts`。
//
// magpie 的做法也是这样：`internal/agent/dsh.go` 的 `dshKindConfig()` 会为每个模型写
// `ReasoningEfforts`（注释原文：「llm-pi-ai makes them pi-ai's thinkingLevelMap, sent as
// reasoning_effort on Chat Completions」）。所以「magpie 能自动填好档位」不是 DSH 会
// 推导，而是 magpie 把配置写进去了 —— 这里做同一件事。
//
// 安全边界：
//   - 只改 `providers.aihub.models` 这一段，其余内容（其他 provider、agent 配置、
//     默认模型）一个字都不动；
//   - 改前先把原文件备份成 `<profile>.bak-hubgo-<时间戳>`；
//   - 写盘用 tmp+rename；
//   - 写完可选跑 `dsh --dump-config --profile <name>` 校验，校验失败自动还原。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dshProfilePath 返回要接管的 DSH profile 文件。可用 AIHUB_DSH_PROFILE 覆盖
// （形如 `web` 或一个具体的 cordis.patch.yml 路径）。
func dshProfilePath() (string, string, error) {
	raw := strings.TrimSpace(os.Getenv("AIHUB_DSH_PROFILE"))
	if raw == "" {
		raw = "web"
	}
	if strings.HasSuffix(raw, ".yml") || strings.HasSuffix(raw, ".yaml") {
		return raw, strings.TrimSuffix(filepath.Base(filepath.Dir(raw)), ""), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".dsh", "profiles", raw, "cordis.patch.yml"), raw, nil
}

// dshPiModelYAML 生成一个模型条目的 YAML（缩进由调用方给）。
type dshPiModel struct {
	ID               string
	Name             string
	ContextWindow    int
	MaxTokens        int
	Input            []string
	ReasoningEfforts map[string]string // pi 档位 → 发给 aihub 的线名；空 map = 不写
}

// buildDshModels 从 aihub 当前目录生成模型清单。
func buildDshModels(h *Hub) []dshPiModel {
	list := aggSnapshot()
	sort.Slice(list, func(i, j int) bool { return list[i].Slug < list[j].Slug })
	out := make([]dshPiModel, 0, len(list))
	for _, am := range list {
		m := dshPiModel{ID: am.Slug}
		m.Name = aggOpenAIDisplayName(h, am)
		if meta, ok := h.modelMeta(am.Line, am.Backend); ok {
			if meta.ContextWindow > 0 {
				m.ContextWindow = meta.ContextWindow
			}
			if meta.MaxInput > 0 {
				m.MaxTokens = meta.MaxInput
			}
		}
		if lineSupportsImage(am.Line) {
			m.Input = []string{"text", "image"}
		} else {
			m.Input = []string{"text"}
		}
		if le, ok := h.effortsOf(am.Line, am.Backend); ok {
			m.ReasoningEfforts = dshEffortsMap(le.Levels)
		}
		out = append(out, m)
	}
	return out
}

// dshEffortsMap 把规范档位翻成 DSH 的 reasoningEfforts 映射。
//
// DSH 认的 pi 档位是 off/minimal/low/medium/high/xhigh/max。这里只把模型真支持的
// 那几档映射成「发给 aihub 的线名」（aihub 会再翻成各线路原生名），其余档位写 null
// —— 写 null 时 DSH 会隐藏该档，不会让用户选到一个上游不认的值。
func dshEffortsMap(levels []string) map[string]string {
	if len(levels) == 0 {
		return nil
	}
	supported := map[string]bool{}
	for _, l := range levels {
		supported[l] = true
	}
	out := map[string]string{}
	for _, l := range canonicalEffortOrder {
		if supported[l] {
			out[l] = l // aihub 内部再翻成线路原生名
		}
	}
	return out
}

// dshModelsYAML 把模型清单渲染成 YAML（models: 之后的内容，含缩进）。
func dshModelsYAML(models []dshPiModel, indent string) string {
	var b strings.Builder
	for _, m := range models {
		fmt.Fprintf(&b, "%s- id: %s\n", indent, yamlScalar(m.ID))
		fmt.Fprintf(&b, "%s  name: %s\n", indent, yamlScalar(m.Name))
		if m.ContextWindow > 0 {
			fmt.Fprintf(&b, "%s  contextWindow: %d\n", indent, m.ContextWindow)
		}
		if m.MaxTokens > 0 {
			fmt.Fprintf(&b, "%s  maxTokens: %d\n", indent, m.MaxTokens)
		}
		if len(m.Input) > 0 {
			fmt.Fprintf(&b, "%s  input:\n", indent)
			for _, in := range m.Input {
				fmt.Fprintf(&b, "%s    - %s\n", indent, in)
			}
		}
		if len(m.ReasoningEfforts) > 0 {
			fmt.Fprintf(&b, "%s  reasoningEfforts:\n", indent)
			keys := make([]string, 0, len(m.ReasoningEfforts))
			for k := range m.ReasoningEfforts {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return canonicalEffortRank(keys[i]) < canonicalEffortRank(keys[j]) })
			for _, k := range keys {
				fmt.Fprintf(&b, "%s    %s: %s\n", indent, k, yamlScalar(m.ReasoningEfforts[k]))
			}
		}
	}
	return b.String()
}

// yamlScalar 给字符串加引号（模型显示名里有 · 与括号、还有中文，加双引号最稳）。
func yamlScalar(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// inAihubBlock 判断第 idx 行是否位于 providers.aihub 段内。
func inAihubBlock(lines []string, idx int) bool {
	for i := idx - 1; i >= 0; i-- {
		switch strings.TrimSpace(lines[i]) {
		case "aihub:":
			for j := i - 1; j >= 0; j-- {
				if strings.TrimSpace(lines[j]) == "providers:" {
					return true
				}
			}
			return false
		case "providers:":
			return false
		}
	}
	return false
}

// parseAihubModelEntries 把现有 aihub 段里的模型条目**结构化**读出来。
//
// 为什么不再原样抓文本：2026-11-03 第一次同步时，原样追加让缩进错位，
// 生成的 profile 直接变成非法 YAML（`bad indentation of a mapping entry`），
// DSH 起不来。现在统一解析成字段再重渲染，输出必然同构、缩进一致。
func parseAihubModelEntries(src string) []dshPiModel {
	lines := strings.Split(src, "\n")
	modelsIdx := -1
	itemIndent := ""
	for i, l := range lines {
		if strings.TrimSpace(l) == "models:" && inAihubBlock(lines, i) {
			modelsIdx = i
			itemIndent = indentOf(l) + "  "
			break
		}
	}
	if modelsIdx < 0 {
		return nil
	}
	var out []dshPiModel
	var cur *dshPiModel
	inInput := false
	inEfforts := false
	effortIndent := ""
	flush := func() {
		if cur != nil && cur.ID != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for i := modelsIdx + 1; i < len(lines); i++ {
		raw := lines[i]
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if indentOf(raw) < itemIndent {
			break
		}
		if indentOf(raw) == itemIndent && strings.HasPrefix(strings.TrimSpace(raw), "- id:") {
			flush()
			id := strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "- id:")), `"`)
			cur = &dshPiModel{ID: id}
			inInput = false
			inEfforts = false
			continue
		}
		if cur == nil {
			continue
		}
		t := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(t, "name:"):
			cur.Name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "name:")), `"`)
			inInput = false
		case strings.HasPrefix(t, "contextWindow:"):
			cur.ContextWindow = atoiSafe(strings.TrimSpace(strings.TrimPrefix(t, "contextWindow:")))
			inInput = false
		case strings.HasPrefix(t, "maxTokens:"):
			cur.MaxTokens = atoiSafe(strings.TrimSpace(strings.TrimPrefix(t, "maxTokens:")))
			inInput = false
		case t == "input:":
			inInput = true
			inEfforts = false
		case t == "reasoningEfforts:":
			inEfforts = true
			inInput = false
			effortIndent = indentOf(raw)
		case inEfforts && strings.Contains(t, ":"):
			if indentOf(raw) <= effortIndent {
				inEfforts = false
				continue
			}
			kv := strings.SplitN(t, ":", 2)
			k := strings.TrimSpace(kv[0])
			v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
			if k != "" && v != "" && v != "null" {
				if cur.ReasoningEfforts == nil {
					cur.ReasoningEfforts = map[string]string{}
				}
				cur.ReasoningEfforts[k] = v
			}
		case inInput && strings.HasPrefix(t, "- "):
			cur.Input = append(cur.Input, strings.TrimSpace(strings.TrimPrefix(t, "- ")))
		default:
			inInput = false
		}
	}
	flush()
	return out
}

// atoiSafe 解析十进制整数，失败返回 0。
func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// replaceAihubModelsBlock 在 profile 文本里定位 `providers:` → `aihub:` → `models:`
// 这一段，用 generated 替换掉它的列表内容。返回替换后的全文。
//
// 只认「providers 下的 aihub 块」，找不到就报错（宁可不动，也不猜别的位置）。
func replaceAihubModelsBlock(src, generated string) (string, error) {
	lines := strings.Split(src, "\n")
	provIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "providers:" {
			provIdx = i
			break
		}
	}
	if provIdx < 0 {
		return "", fmt.Errorf("profile 里没找到 providers: 段")
	}
	provIndent := indentOf(lines[provIdx])

	// 在 providers 段内找 aihub:
	aihubIdx := -1
	for i := provIdx + 1; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if indentOf(l) <= provIndent {
			break // 出了 providers 段
		}
		if trimmed == "aihub:" {
			aihubIdx = i
			break
		}
	}
	if aihubIdx < 0 {
		return "", fmt.Errorf("providers 下没找到 aihub: 块（DSH 里还没配过这个网关？先在 DSH 设置页加一次再来接管）")
	}
	aihubIndent := indentOf(lines[aihubIdx])

	// 在 aihub 段内找 models:
	modelsIdx := -1
	for i := aihubIdx + 1; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		if indentOf(l) <= aihubIndent {
			break // 出了 aihub 块
		}
		if trimmed == "models:" {
			modelsIdx = i
			break
		}
	}
	if modelsIdx < 0 {
		return "", fmt.Errorf("aihub 块里没找到 models:")
	}
	modelsIndent := indentOf(lines[modelsIdx])

	// 找到 models 列表的结束位置：第一个缩进 <= modelsIndent 的非空行
	end := len(lines)
	for i := modelsIdx + 1; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if indentOf(l) <= modelsIndent {
			end = i
			break
		}
	}

	itemIndent := modelsIndent + "  " // 与 profile 既有写法一致：models: 下 2 空格起条目
	if len(generated) > 0 && !strings.HasSuffix(generated, "\n") {
		generated += "\n"
	}
	if generated == "" {
		// 没有模型：保留一个空列表占位（`models: []`），别留悬空键
		generated = modelsIndent + "  []\n"
	} else {
		generated = adjustIndent(generated, itemIndent)
	}

	out := make([]string, 0, len(lines))
	out = append(out, lines[:modelsIdx+1]...)
	out = append(out, strings.Split(strings.TrimSuffix(generated, "\n"), "\n")...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}

// adjustIndent 把生成的内容改成以 itemIndent 开头（生成时用的是占位缩进）。
func adjustIndent(generated, itemIndent string) string {
	lines := strings.Split(strings.TrimSuffix(generated, "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, itemIndent+strings.TrimPrefix(l, "  "))
	}
	return strings.Join(out, "\n") + "\n"
}

// indentOf 返回行首空格数（tab 按 2 个空格算，本仓库 profile 全是空格）。
func indentOf(s string) string {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
			continue
		}
		if r == '\t' {
			n += 2
			continue
		}
		break
	}
	return strings.Repeat(" ", n)
}

// takeOverDSH 生成模型清单并写进 DSH profile（改前备份、写后校验）。
func takeOverDSH(h *Hub) (string, error) {
	path, profile, err := dshProfilePath()
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读不到 DSH profile（%s）：%w", path, err)
	}
	models := buildDshModels(h)
	if len(models) == 0 {
		return "", fmt.Errorf("aihub 聚合白名单是空的，没有可写的模型")
	}
	// 目录里没有、但用户配置里有的模型（早先手加的、或该线路暂时没进聚合白名单）：
	// 结构化保留，绝不静默删除 —— 2026-11-03 第一次同步就因此丢了
	// intl.deepseek-v4.1-flash，而 agent-default-model 正指着它。
	inCatalog := map[string]bool{}
	for _, m := range models {
		inCatalog[m.ID] = true
	}
	kept := parseAihubModelEntries(string(src))
	// 字段级兜底：目录这次给不出档位/窗口时，保留用户配置里已有的那份。
	// （2026-11-03 实测：同步时 qoder 线路没在跑 → 目录里没有它的档位，
	//   覆盖后 qoder.Qwen3.8-Flash 的 low/medium/xhigh 就没了。）
	existing := map[string]dshPiModel{}
	for _, m := range kept {
		existing[m.ID] = m
	}
	for i := range models {
		old, ok := existing[models[i].ID]
		if !ok {
			continue
		}
		if len(models[i].ReasoningEfforts) == 0 && len(old.ReasoningEfforts) > 0 {
			models[i].ReasoningEfforts = old.ReasoningEfforts
		}
		if models[i].ContextWindow == 0 {
			models[i].ContextWindow = old.ContextWindow
		}
		if models[i].MaxTokens == 0 {
			models[i].MaxTokens = old.MaxTokens
		}
		if len(models[i].Input) == 0 {
			models[i].Input = old.Input
		}
	}
	var extras []dshPiModel
	for _, m := range kept {
		if inCatalog[m.ID] {
			continue
		}
		extras = append(extras, m)
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].ID < extras[j].ID })
	models = append(models, extras...)

	body := strings.TrimSuffix(dshModelsYAML(models, "  "), "\n")
	next, err := replaceAihubModelsBlock(string(src), body)
	if err != nil {
		return "", err
	}
	if next == string(src) {
		return "DSH 配置已是最新，无需改动", nil
	}

	stamp := time.Now().Format("20060102-150405")
	bak := fmt.Sprintf("%s.bak-hubgo-%s", path, stamp)
	if err := os.WriteFile(bak, src, 0o600); err != nil {
		return "", fmt.Errorf("备份失败，未改动：%w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(next), 0o600); err != nil {
		return "", fmt.Errorf("写临时文件失败：%w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("替换 profile 失败：%w", err)
	}
	// 用 dsh 自己校验一遍 —— 它是唯一权威的 YAML 解析器。
	// 2026-11-03 第一次同步就因为缩进写坏，把 profile 变成非法 YAML；
	// 那次是手工回滚的，现在这一步必须自动化。
	if err := validateDshProfile(profile); err != nil {
		_ = os.WriteFile(path, src, 0o600)
		return "", fmt.Errorf("写完后 dsh 校验失败，已回滚原文件：%w", err)
	}
	return fmt.Sprintf("已接管 DSH（profile=%s）：写入 %d 个模型（含思考档位），原文件备份为 %s",
		profile, len(models), filepath.Base(bak)), nil
}

// restoreDSHLatest 用最近的备份还原（接管出错时的退路）。
func restoreDSHLatest() (string, error) {
	path, _, err := dshProfilePath()
	if err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(path + ".bak-hubgo-*")
	if len(matches) == 0 {
		return "", fmt.Errorf("没有找到接管前的备份（%s.bak-hubgo-*）", path)
	}
	sort.Strings(matches)
	newest := matches[len(matches)-1]
	b, err := os.ReadFile(newest)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return "已从备份还原 DSH 配置：" + filepath.Base(newest), nil
}

// dshTakeoverStatus 给控制台显示当前状态（是否已接管/备份数）。
func dshTakeoverStatus() map[string]any {
	path, profile, err := dshProfilePath()
	out := map[string]any{"profile": profile, "path": path}
	if err != nil {
		out["ok"] = false
		out["msg"] = err.Error()
		return out
	}
	if _, err := os.Stat(path); err != nil {
		out["ok"] = false
		out["msg"] = "找不到 profile 文件"
		return out
	}
	matches, _ := filepath.Glob(path + ".bak-hubgo-*")
	out["ok"] = true
	out["backups"] = len(matches)
	return out
}

// validateDshProfile 调 `dsh --dump-config --profile <name>` 校验配置能否解析。
// 找不到 dsh 就跳过（测试环境常见）；解析失败返回错误。
func validateDshProfile(profile string) error {
	bin, err := exec.LookPath("dsh")
	if err != nil {
		return nil // 没装 dsh 就无从校验，不阻塞
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--dump-config", "--profile", profile)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 600 {
			msg = msg[:600] + "…"
		}
		return fmt.Errorf("%v：%s", err, msg)
	}
	return nil
}
