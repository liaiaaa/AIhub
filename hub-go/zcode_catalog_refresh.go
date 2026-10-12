package main

// 实时模型目录（2026-09-27 接入 zcode.z.ai/api/v1/client/configs，替代
// 「硬编码快照是唯一目录」的旧模式）。
//
// 该端点公开可读（探针 C/D 实测 HTTP 200，无需凭据），返回
// data.builtinModels + data.providers[].models，字段比官方文档更新更勤：
//   modelId / name / contextWindow / maxCompletionTokens / priority
//   capabilities.vision / modalities.input
//   reasoning.levels{low,medium,high,max}（各 wire 的档位映射）+ defaultLevel
//
// 合并策略（fail-open）：
//   远端成功 → 以远端为主体，用内置快照补齐远端没有的元数据（如 5.3 系
//   「思考不可关闭」这一文档结论），再追加快照独有模型，整体替换目录；
//   远端失败 → 保留现有目录不动，30s 冷却；成功后 30min 内不重复拉取。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	zcodeCatalogOrigin     = "https://zcode.z.ai"
	zcodeCatalogSuccessTTL = 30 * time.Minute
	zcodeCatalogFailCool   = 30 * time.Second
)

// zcodeRemoteModel client/configs 里的模型条目（只取需要的字段）。
type zcodeRemoteModel struct {
	ModelID             string `json:"modelId"`
	Name                string `json:"name"`
	ContextWindow       int    `json:"contextWindow"`
	Priority            int    `json:"priority"`
	MaxCompletionTokens int    `json:"maxCompletionTokens"`
	Capabilities        struct {
		Vision bool `json:"vision"`
	} `json:"capabilities"`
	Reasoning struct {
		Levels       map[string]json.RawMessage `json:"levels"`
		DefaultLevel string                     `json:"defaultLevel"`
	} `json:"reasoning"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
}

type zcodeRemoteProvider struct {
	ID     string             `json:"id"`
	Schema string             `json:"schema"`
	Models []zcodeRemoteModel `json:"models"`
}

type zcodeClientConfigs struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Providers     []zcodeRemoteProvider `json:"providers"`
		BuiltinModels []zcodeRemoteModel    `json:"builtinModels"`
	} `json:"data"`
}

// zcodeEffortOrder 档位规范序（目录透出时保持该顺序）。
var zcodeEffortOrder = []string{"low", "medium", "high", "max"}

// zcodeRemoteToEntry 远端条目 → 目录条目（不查快照的基础映射）。
func zcodeRemoteToEntry(m zcodeRemoteModel) (zcodeModelEntry, bool) {
	key := strings.ToLower(strings.TrimSpace(m.ModelID))
	if key == "" {
		return zcodeModelEntry{}, false
	}
	e := zcodeModelEntry{
		Key:             key,
		DisplayName:     strings.TrimSpace(m.ModelID),
		Vendor:          "zai",
		MaxInputTokens:  m.ContextWindow,
		MaxOutputTokens: m.MaxCompletionTokens,
	}
	if e.DisplayName == "" {
		e.DisplayName = m.Name
	}
	e.IsVision = m.Capabilities.Vision
	for _, mod := range m.Modalities.Input {
		if mod == "image" || mod == "video" {
			e.IsVision = true
		}
	}
	e.IsReasoning = len(m.Reasoning.Levels) > 0
	for _, lvl := range zcodeEffortOrder {
		if _, ok := m.Reasoning.Levels[lvl]; ok {
			e.Efforts = append(e.Efforts, lvl)
		}
	}
	e.DefaultEffort = m.Reasoning.DefaultLevel
	if m.ContextWindow > 0 {
		e.ContextWindows = []string{zcodeHumanizeTokens(m.ContextWindow)}
	}
	return e, true
}

// mergeZcodeCatalog 远端目录为主体，快照补元数据、补独有模型。
// 纯函数（无锁无 IO），替换现有目录前可整体单测。
func mergeZcodeCatalog(remote, snapshot []zcodeModelEntry) []zcodeModelEntry {
	snapByKey := make(map[string]zcodeModelEntry, len(snapshot))
	for _, s := range snapshot {
		snapByKey[s.Key] = s
	}
	defaultKey := zcodeDefaultKey(snapshot)

	merged := make([]zcodeModelEntry, 0, len(remote)+len(snapshot))
	seen := make(map[string]bool, len(remote)+len(snapshot))
	for _, r := range remote {
		key := r.Key
		if seen[key] {
			continue
		}
		seen[key] = true
		if s, ok := snapByKey[key]; ok {
			// 快照里有更权威/远端缺失的元数据：思考可关性、档位、默认档
			if len(r.Efforts) == 0 && len(s.Efforts) > 0 {
				r.Efforts = s.Efforts
			}
			if r.DefaultEffort == "" {
				r.DefaultEffort = s.DefaultEffort
			}
			r.SupportsDisable = s.SupportsDisable
			r.IsReasoning = r.IsReasoning || s.IsReasoning
			r.IsVision = r.IsVision || s.IsVision
			if r.MaxInputTokens == 0 {
				r.MaxInputTokens = s.MaxInputTokens
			}
			if r.MaxOutputTokens == 0 {
				r.MaxOutputTokens = s.MaxOutputTokens
			}
			if len(r.ContextWindows) == 0 {
				r.ContextWindows = s.ContextWindows
			}
		}
		r.IsDefault = key == defaultKey
		merged = append(merged, r)
	}
	// 快照独有模型保序追加（远端未列出 ≠ 下线，未收录仍可透传，但目录里留着）
	for _, s := range snapshot {
		if !seen[s.Key] {
			seen[s.Key] = true
			merged = append(merged, s)
		}
	}
	// 默认模型排最前，其余维持「远端序 + 快照独有」的稳定顺序
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].IsDefault && !merged[j].IsDefault
	})
	return merged
}

// refreshCatalog 拉取远端目录并替换（带节流）。返回本册条数与错误。
func (p *ZcodeProxy) refreshCatalog() (int, error) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	now := time.Now()
	if !p.catalogLiveAt.IsZero() && now.Sub(p.catalogLiveAt) < zcodeCatalogSuccessTTL {
		return len(p.catalog), nil
	}
	if !p.catalogTryAt.IsZero() && now.Sub(p.catalogTryAt) < zcodeCatalogFailCool {
		return len(p.catalog), nil
	}
	p.catalogTryAt = now

	entries, err := p.fetchRemoteCatalog()
	if err != nil {
		return len(p.catalog), err
	}
	merged := mergeZcodeCatalog(entries, p.catalog)
	p.catalog = merged
	p.aliases = buildZcodeAliases(merged)
	p.catalogLiveAt = now
	return len(merged), nil
}

// fetchRemoteCatalog GET client/configs 并转成目录条目。
func (p *ZcodeProxy) fetchRemoteCatalog() ([]zcodeModelEntry, error) {
	id := p.identity
	if id == nil {
		id = &zcodeIdentity{platform: zcodeNodePlatform(), arch: zcodeNodeArch(), appVersion: "3.14.0"}
	}
	url := fmt.Sprintf("%s/api/v1/client/configs?app_version=%s&platform=%s",
		zcodeCatalogOrigin, id.appVersion, id.platformParam())

	client := &http.Client{Transport: directClient().Transport, Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	setIdentityHeaders(req, id.contextHeaders())

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("client/configs HTTP %d", resp.StatusCode)
	}
	var parsed zcodeClientConfigs
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("client/configs code=%d msg=%s", parsed.Code, parsed.Msg)
	}

	entries := make([]zcodeModelEntry, 0, 16)
	// builtinModels 优先（桌面端内置目录，含 5.3 系与 reasoning 档位）
	for _, m := range parsed.Data.BuiltinModels {
		if e, ok := zcodeRemoteToEntry(m); ok {
			entries = append(entries, e)
		}
	}
	// providers 补齐（openai:chat / anthropic 通道的模型都会并入）
	for _, prov := range parsed.Data.Providers {
		for _, m := range prov.Models {
			if e, ok := zcodeRemoteToEntry(m); ok {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("client/configs 无模型条目")
	}
	return entries, nil
}

// zcodeHumanizeTokens 上下文窗口人类可读形态：整除优先（1M/200K），
// 非整除十进制取整（128000→128K）。
func zcodeHumanizeTokens(n int) string {
	switch {
	case n >= 1_000_000:
		if n%1_000_000 == 0 {
			return strconv.Itoa(n/1_000_000) + "M"
		}
		if n%(1<<20) == 0 {
			return strconv.Itoa(n/(1<<20)) + "M"
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		if n%1000 == 0 { // 十进制整（128000→128K）优先于二进制整
			return strconv.Itoa(n/1000) + "K"
		}
		if n%1024 == 0 { // 二进制整（204800→200K、131072→128K）
			return strconv.Itoa(n/1024) + "K"
		}
		return strconv.Itoa(n/1000) + "K"
	default:
		return strconv.Itoa(n)
	}
}
