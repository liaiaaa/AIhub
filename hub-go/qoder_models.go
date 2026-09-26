package main

// Qoder CN 模型目录（单一事实源）。
//
// 背景：Qoder 网关的 `model` 字段用的是**内部 key**（如 qfmodel / gmodel / dmodel），
// 而不是界面上的显示名（Qwen3.8-Flash / GLM-5.3 / DeepSeek-V4-Pro）。官方客户端
// 发请求时用的也是 key——已从 CLI 日志实测确认（SSEStreamDiagnostic ... model=qfmodel）。
//
// 权威来源（2026-09-23 实测，两个独立来源交叉验证，结果完全一致）：
//  1. GET https://gateway.qoder.com.cn/algo/api/v2/model/list —— 官方模型目录接口，
//     返回 chat/developer/assistant/... 多组清单，含 key、display_name、能力与价格。
//  2. 桌面端 %APPDATA%\com.qodercn.app.stable\main.sqlite 的 chat_model_preferences 表，
//     记录用户实际可见的 14 个 model_key。
//
// 因此本文件同时提供「内置快照」（离线兜底）与「动态拉取」（跟随官方上新）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// QoderModelListURL 官方模型目录接口（COSY 签名，GET，body 为空）。
const QoderModelListURL = "https://gateway.qoder.com.cn/algo/api/v2/model/list"

// qoderCatalogGroup 动态拉取时采用的清单分组。
// chat 组包含全部 14 个模型，与桌面端 chat_model_preferences 表完全一致。
const qoderCatalogGroup = "chat"

// qoderModelEntry 目录中的一个模型。
type qoderModelEntry struct {
	Key            string   `json:"key"`          // 真实 key，直接写进上游 body 的 model 字段
	DisplayName    string   `json:"display_name"` // 官方显示名，作为对外别名
	Vendor         string   `json:"vendor"`       // 厂商，用于 /v1/models 的 owned_by
	PriceFactor    float64  `json:"price_factor"`
	IsFree         bool     `json:"is_free"`
	IsReasoning    bool     `json:"is_reasoning"`
	IsVision       bool     `json:"is_vl"`
	IsDefault      bool     `json:"is_default"`
	MaxInputTokens int      `json:"max_input_tokens"`
	ContextWindows []string `json:"context_windows,omitempty"`

	// 单次回复的输出 token 上限（官方目录 max_output_tokens；缺省按 32768 处理）。
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`

	// 思考强度（thinking_config）：官方 UI“思考强度”入口的真实数据源。
	// Efforts 为该模型支持的强度档（none/low/medium/high/xhigh/max 子集，规范序），
	// DefaultEffort 为默认档，SupportsDisabled 表示能否整体关闭思考。
	Efforts          []string `json:"efforts,omitempty"`
	DefaultEffort    string   `json:"default_effort,omitempty"`
	SupportsDisabled bool     `json:"supports_disabled,omitempty"`

	// ContextConfig 上下文档位（官方 context_config）：{档名: {token_count, is_default}}。
	// 请求侧用它选择上下文档（默认取最大档，见 qoderPickContextTier）。
	ContextConfig map[string]qoderContextTier `json:"context_config,omitempty"`
}

// qoderContextTier 一个上下文档位。
type qoderContextTier struct {
	TokenCount int  `json:"token_count"`
	IsDefault  bool `json:"is_default"`
}

// defaultQoderCatalog 内置快照（2026-09-23 实测；思考强度字段 2026-09-24 补录，
// 来自官方 /algo/api/v2/model/list 的 thinking_config；2026-09-26 补上下文档位
// context_config，档位标签按官方体系 200K/400K/1M 换算成 token 数）。
// 启动时若无法从官方接口刷新，则用这份数据兜底。
func defaultQoderCatalog() []qoderModelEntry {
	out := []qoderModelEntry{
		{Key: "auto", DisplayName: "Auto", Vendor: "qoder-cn", PriceFactor: 0.5,
			IsReasoning: true, IsVision: true, IsDefault: true, MaxInputTokens: 200000},
		{Key: "qmodel_38max", DisplayName: "Qwen3.8-Max", Vendor: "alibaba", PriceFactor: 0.5,
			IsFree: true, IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", SupportsDisabled: true},
		{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", Vendor: "alibaba", PriceFactor: 0,
			IsFree: true, IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", SupportsDisabled: true},
		{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", Vendor: "alibaba", PriceFactor: 0.5,
			IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"}, SupportsDisabled: true},
		{Key: "qmodel", DisplayName: "Qwen3.7-Plus", Vendor: "alibaba", PriceFactor: 0.1,
			IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"}, SupportsDisabled: true},
		{Key: "q37fmodel", DisplayName: "Qwen3.7-Flash", Vendor: "alibaba", PriceFactor: 0.1,
			IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"}},
		{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", Vendor: "deepseek", PriceFactor: 0.5,
			IsReasoning: true, IsVision: true, MaxInputTokens: 96000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"high", "max"}, DefaultEffort: "max", SupportsDisabled: true},
		{Key: "dfmodel", DisplayName: "DeepSeek-Flash", Vendor: "deepseek", PriceFactor: 0.1,
			IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"low", "high", "max"}, DefaultEffort: "max", SupportsDisabled: true},
		{Key: "gmodel", DisplayName: "GLM-5.3", Vendor: "zhipu", PriceFactor: 0.8,
			IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"low", "high", "max"}, DefaultEffort: "max"},
		{Key: "gfmodel", DisplayName: "GLM-5.3-Flash", Vendor: "zhipu", PriceFactor: 0.1,
			IsReasoning: true, IsVision: true, MaxInputTokens: 1000000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"high", "max"}, DefaultEffort: "max"},
		{Key: "gm51model", DisplayName: "GLM-5.2", Vendor: "zhipu", PriceFactor: 0.6,
			IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"high", "max"}, DefaultEffort: "max", SupportsDisabled: true},
		{Key: "kmodel_latest", DisplayName: "Kimi-K3", Vendor: "moonshot", PriceFactor: 1.4,
			IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"low", "high", "max"}, DefaultEffort: "max"},
		{Key: "kmodel", DisplayName: "Kimi-K2.8-Preview", Vendor: "moonshot", PriceFactor: 0.8,
			IsReasoning: true, IsVision: true, MaxInputTokens: 180000,
			ContextWindows: []string{"200K", "400K", "1M"},
			Efforts:        []string{"low", "high", "max"}, DefaultEffort: "max"},
		{Key: "mmodel", DisplayName: "MiniMax-M2.7", Vendor: "minimax", PriceFactor: 0.2,
			MaxInputTokens: 180000, ContextWindows: []string{"200K"}},
	}
	// 内置快照统一补上下文档位（动态刷新后会用官方真值覆盖）
	for i := range out {
		if len(out[i].ContextConfig) == 0 && len(out[i].ContextWindows) > 0 {
			out[i].ContextConfig = qoderContextConfigFromWindows(out[i].ContextWindows)
		}
	}
	return out
}

// qoderVendorFor 从显示名推断厂商（动态拉取时官方不返回厂商字段）。
func qoderVendorFor(display string) string {
	d := strings.ToLower(strings.TrimSpace(display))
	switch {
	case d == "" || d == "auto":
		return "qoder-cn"
	case strings.HasPrefix(d, "qwen"):
		return "alibaba"
	case strings.HasPrefix(d, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(d, "glm"):
		return "zhipu"
	case strings.HasPrefix(d, "kimi"):
		return "moonshot"
	case strings.HasPrefix(d, "minimax"):
		return "minimax"
	}
	return "qoder-cn"
}

// ---------- 官方目录响应解析 ----------

type qoderRemoteModel struct {
	Key         string  `json:"key"`
	DisplayName string  `json:"display_name"`
	Enable      *bool   `json:"enable"`
	IsVL        bool    `json:"is_vl"`
	IsReasoning bool    `json:"is_reasoning"`
	IsDefault   bool    `json:"is_default"`
	IsFree      bool    `json:"is_free"`
	PriceFactor float64 `json:"price_factor"`
	MaxInput    int     `json:"max_input_tokens"`
	MaxOutput   int     `json:"max_output_tokens"`
	ContextCfg  map[string]struct {
		TokenCount int  `json:"token_count"`
		IsDefault  bool `json:"is_default"`
	} `json:"context_config"`
	ThinkingConfig json.RawMessage `json:"thinking_config"`
}

// qoderThinkingSide thinking_config 的 enabled/disabled 一侧。
type qoderThinkingSide struct {
	IsDefault bool `json:"is_default"`
	Efforts   map[string]struct {
		IsDefault bool `json:"is_default"`
	} `json:"efforts"`
}

// qoderParseThinkingConfig 解析官方 thinking_config：
// enabled.efforts → 支持档位（规范序），is_default 档 → 默认档，
// disabled 键存在 → 支持整体关闭。缺失/无档位时全部留空。
func qoderParseThinkingConfig(raw json.RawMessage) (efforts []string, defaultEffort string, supportsDisabled bool) {
	if len(raw) == 0 {
		return nil, "", false
	}
	var cfg map[string]qoderThinkingSide
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, "", false
	}
	if _, ok := cfg["disabled"]; ok {
		supportsDisabled = true
	}
	en, ok := cfg["enabled"]
	if !ok {
		return nil, "", supportsDisabled
	}
	for _, order := range qoderEffortOrder {
		if _, ok := en.Efforts[order]; ok {
			efforts = append(efforts, order)
		}
	}
	for name, opt := range en.Efforts {
		if opt.IsDefault {
			if e := qoderNormalizeEffort(name); e != "" && e != "none" {
				defaultEffort = e
			}
		}
	}
	return efforts, defaultEffort, supportsDisabled
}

// parseQoderModelList 解析官方 /algo/api/v2/model/list 响应。
// 只取 chat 分组；若缺失则回退到第一个非空分组。
func parseQoderModelList(raw []byte) ([]qoderModelEntry, error) {
	var groups map[string]json.RawMessage
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("解析模型目录 JSON 失败：%w", err)
	}

	var list []qoderRemoteModel
	if rawChat, ok := groups[qoderCatalogGroup]; ok {
		if err := json.Unmarshal(rawChat, &list); err != nil {
			return nil, fmt.Errorf("解析 %s 分组失败：%w", qoderCatalogGroup, err)
		}
	}
	if len(list) == 0 {
		// 分组名变动时的兜底：取第一个非空数组
		names := make([]string, 0, len(groups))
		for name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			var cand []qoderRemoteModel
			if json.Unmarshal(groups[name], &cand) == nil && len(cand) > 0 {
				list = cand
				break
			}
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("模型目录为空（分组：%d 个）", len(groups))
	}

	out := make([]qoderModelEntry, 0, len(list))
	for _, m := range list {
		key := strings.TrimSpace(m.Key)
		if key == "" {
			continue
		}
		if m.Enable != nil && !*m.Enable {
			continue // 官方已下架
		}
		display := strings.TrimSpace(m.DisplayName)
		if display == "" {
			display = key
		}
		windows := make([]string, 0, len(m.ContextCfg))
		for name := range m.ContextCfg {
			windows = append(windows, name)
		}
		sort.Strings(windows)
		efforts, defaultEffort, supportsDisabled := qoderParseThinkingConfig(m.ThinkingConfig)

		// 上下文档位：官方 context_config 优先；缺失时从档位标签推（内置快照路径）
		ctxCfg := make(map[string]qoderContextTier, len(m.ContextCfg))
		for name, t := range m.ContextCfg {
			ctxCfg[name] = qoderContextTier{TokenCount: t.TokenCount, IsDefault: t.IsDefault}
		}
		if len(ctxCfg) == 0 {
			ctxCfg = qoderContextConfigFromWindows(windows)
		}

		out = append(out, qoderModelEntry{
			Key:              key,
			DisplayName:      display,
			Vendor:           qoderVendorFor(display),
			PriceFactor:      m.PriceFactor,
			IsFree:           m.IsFree,
			IsReasoning:      m.IsReasoning,
			IsVision:         m.IsVL,
			IsDefault:        m.IsDefault,
			MaxInputTokens:   m.MaxInput,
			MaxOutputTokens:  m.MaxOutput,
			ContextWindows:   windows,
			ContextConfig:    ctxCfg,
			Efforts:          efforts,
			DefaultEffort:    defaultEffort,
			SupportsDisabled: supportsDisabled,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("模型目录无有效条目")
	}
	return out, nil
}

// ---------- 别名表 ----------

// qoderNormalizeAlias 归一化模型名，用于宽松匹配：
// 小写、去空白、下划线转连字符（于是 kmodel_latest 与 kmodel-latest 都能命中）。
func qoderNormalizeAlias(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\t", "")
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

// buildQoderAliases 构建「别名 → 真实 key」映射。
// 双轨：真实 key 与官方显示名都可直接调用，统一归一到 key。
func buildQoderAliases(catalog []qoderModelEntry) map[string]string {
	aliases := make(map[string]string, len(catalog)*3)
	add := func(alias, key string) {
		n := qoderNormalizeAlias(alias)
		if n == "" {
			return
		}
		if _, exists := aliases[n]; !exists {
			aliases[n] = key // 先注册者优先（key 优先于显示名）
		}
	}
	// 第一轮：真实 key
	for _, m := range catalog {
		add(m.Key, m.Key)
	}
	// 第二轮：显示名 → key
	for _, m := range catalog {
		add(m.DisplayName, m.Key)
	}
	// 第三轮：少量人工别名（兼容常见叫法）
	for _, extra := range []struct{ alias, key string }{
		{"qwen3.8-flash", "qfmodel"},
		{"qwen-3.8-flash", "qfmodel"},
		{"qwen3.8-flash-free", "qfmodel"},
		{"glm-5.3-flash", "gfmodel"},
		{"glm5.3", "gmodel"},
		{"glm5.2", "gm51model"},
		{"deepseek-v4", "dmodel"},
		{"deepseek-v4-pro", "dmodel"},
		{"kimi-k3", "kmodel_latest"},
		{"kimi-k2.8", "kmodel"},
		{"minimax-m2.7", "mmodel"},
		{"default", "qfmodel"},
	} {
		add(extra.alias, extra.key)
	}
	return aliases
}

// qoderDefaultKey 从目录里挑默认模型：优先 qfmodel（Qwen3.8-Flash，免费），
// 其次官方 is_default 项，最后取第一个。
func qoderDefaultKey(catalog []qoderModelEntry) string {
	for _, m := range catalog {
		if m.Key == "qfmodel" {
			return m.Key
		}
	}
	for _, m := range catalog {
		if m.IsDefault {
			return m.Key
		}
	}
	if len(catalog) > 0 {
		return catalog[0].Key
	}
	return "qfmodel"
}

// qoderContextConfigFromWindows 从档位标签推 ContextConfig（内置快照兜底）。
// 只认已知标签：200K/400K/1M（官方目录的档位体系）。
func qoderContextConfigFromWindows(windows []string) map[string]qoderContextTier {
	known := map[string]int{"200K": 200000, "400K": 400000, "1M": 1000000}
	out := make(map[string]qoderContextTier, len(windows))
	for _, w := range windows {
		if n, ok := known[w]; ok {
			out[w] = qoderContextTier{TokenCount: n}
		}
	}
	return out
}

// qoderPickContextTier 选定请求使用的上下文档：QODER_CONTEXT_TIER 显式指定
// （"none" = 不发 context_config，旧行为）；默认取该模型可用的最大档
// （用户 2026-09-26 拍板：DSH 会话按 1M 配，代理不降档）。
// 返回完整 context_config 对象（选定档 is_default=true，其余 false）；
// 模型无档位信息时返回 nil（不发送）。
func qoderPickContextTier(entry *qoderModelEntry) map[string]any {
	if entry == nil || len(entry.ContextConfig) == 0 {
		return nil
	}
	if v := strings.TrimSpace(os.Getenv("QODER_CONTEXT_TIER")); v != "" {
		if strings.EqualFold(v, "none") {
			return nil
		}
		if _, ok := entry.ContextConfig[v]; !ok {
			return nil // 指定了该模型没有的档：宁可不发，不发非法值
		}
		out := make(map[string]any, len(entry.ContextConfig))
		for name, t := range entry.ContextConfig {
			out[name] = map[string]any{"token_count": t.TokenCount, "is_default": name == v}
		}
		return out
	}
	// 默认：token 数最大的档
	best := ""
	bestN := -1
	for name, t := range entry.ContextConfig {
		if t.TokenCount > bestN {
			best, bestN = name, t.TokenCount
		}
	}
	if best == "" {
		return nil
	}
	out := make(map[string]any, len(entry.ContextConfig))
	for name, t := range entry.ContextConfig {
		out[name] = map[string]any{"token_count": t.TokenCount, "is_default": name == best}
	}
	return out
}

// ---------- 动态拉取 ----------

// fetchQoderCatalog 用 COSY 签名调用官方目录接口。
func fetchQoderCatalog(ctx context.Context, auth *QoderAuth, client *http.Client, url string) ([]qoderModelEntry, error) {
	if auth == nil {
		return nil, fmt.Errorf("凭据未就绪")
	}
	// 目录接口是 GET，body 为空——签名串里的 body 段必须同为 ""
	headers, err := auth.Sign(url, "")
	if err != nil {
		return nil, fmt.Errorf("签名失败：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json") // 覆盖默认的 text/event-stream

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求模型目录失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("模型目录返回 HTTP %d", resp.StatusCode)
	}
	// 目录约 70KB，限读 8MB 足够且可防御异常响应
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取模型目录失败：%w", err)
	}
	return parseQoderModelList(body)
}

// refreshCatalog 异步刷新目录；失败则保留现有（内置）快照。
// 绝不阻塞或影响代理启动。
func (p *QoderProxy) refreshCatalog() {
	if p.auth == nil || p.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	entries, err := fetchQoderCatalog(ctx, p.auth, p.client, QoderModelListURL)
	if err != nil {
		if p.log != nil {
			p.log.Log("模型目录动态刷新失败，沿用内置快照（%d 个模型）：%v", len(p.catalogSnapshot()), err)
		}
		return
	}
	p.setCatalog(entries)
	if p.log != nil {
		p.log.Log("模型目录已从官方接口刷新：%d 个模型（%s）", len(entries), qoderCatalogSummary(entries))
	}
}

// qoderCatalogSummary 生成简短摘要，便于日志排查。
func qoderCatalogSummary(catalog []qoderModelEntry) string {
	parts := make([]string, 0, len(catalog))
	for _, m := range catalog {
		parts = append(parts, m.Key+"="+m.DisplayName)
	}
	return strings.Join(parts, ", ")
}
