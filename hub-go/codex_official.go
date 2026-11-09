package main

// Codex 官方模型目录（A′ 方案，2026-11-09）。
//
// 背景：实测本机 Codex 0.167 的模型选择器目录只来自 model_catalog_json 或
// 内置 bundled 目录，**不会**去请求 openai_base_url 的 /models（见
// docs/superpowers/specs/2026-11-09-codex-mixed-model-catalog-design.md 第 1.1 节）。
// 所以官方条目改从 `codex debug models --bundled` 取：只读、不联网、
// 不需要登录态、不消耗官方额度。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

const codexOfficialSlugsFile = "codex_official_slugs.json"

// codexBinPath 本机 Codex CLI 路径；测试用 AIHUB_CODEX_BIN 注入假二进制。
func codexBinPath() string {
	if p := strings.TrimSpace(os.Getenv("AIHUB_CODEX_BIN")); p != "" {
		return p
	}
	return "/usr/lib/chatgpt/resources/codex"
}

// codexBundledJSON 取内置目录原文。做成变量便于测试替换。
var codexBundledJSON = func() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, codexBinPath(), "debug", "models", "--bundled").Output()
	if err != nil {
		return nil, fmt.Errorf("运行 codex debug models --bundled 失败：%w", err)
	}
	return out, nil
}

// codexOfficialEntries 解析内置目录；visibility=list 在前、hide 在后。
func codexOfficialEntries() ([]map[string]any, error) {
	raw, err := codexBundledJSON()
	if err != nil {
		return nil, err
	}
	var doc struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析内置目录失败：%w", err)
	}
	out := make([]map[string]any, 0, len(doc.Models))
	for _, e := range doc.Models {
		if s, _ := e["slug"].(string); s != "" {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("内置目录为空或没有可用 slug")
	}
	sort.SliceStable(out, func(i, j int) bool {
		return codexVisibilityRank(out[i]) < codexVisibilityRank(out[j])
	})
	return out, nil
}

func codexVisibilityRank(e map[string]any) int {
	if v, _ := e["visibility"].(string); v == "hide" {
		return 1
	}
	return 0
}

// codexOfficialSlugList 取条目里的 slug 列表（保持顺序）。
func codexOfficialSlugList(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if s, _ := e["slug"].(string); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// codexOfficialSlugsPath 官方 slug 集合落盘位置（数据根，不进家目录）。
func codexOfficialSlugsPath() string { return dataPath(codexOfficialSlugsFile) }

var (
	officialSlugMu    sync.RWMutex
	officialSlugCache map[string]bool
)

// saveCodexOfficialSlugs 落盘并刷新内存缓存。0600：内容是官方模型名单。
func saveCodexOfficialSlugs(slugs []string) error {
	b, err := json.MarshalIndent(map[string]any{"slugs": slugs}, "", " ")
	if err != nil {
		return err
	}
	path := codexOfficialSlugsPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	setOfficialSlugCache(slugs)
	return nil
}

// loadCodexOfficialSlugs 读盘；文件缺失/损坏返回 nil（不报错）。
func loadCodexOfficialSlugs() []string {
	b, err := os.ReadFile(codexOfficialSlugsPath())
	if err != nil {
		return nil
	}
	var doc struct {
		Slugs []string `json:"slugs"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return doc.Slugs
}

func setOfficialSlugCache(slugs []string) {
	m := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		if s != "" {
			m[s] = true
		}
	}
	officialSlugMu.Lock()
	officialSlugCache = m
	officialSlugMu.Unlock()
}

// codexOfficialSlugSet 取官方 slug 集合：内存 → 磁盘 → 现场只读重算。
// 只读重算不落盘（落盘只发生在生成混合目录时）。
func codexOfficialSlugSet() map[string]bool {
	officialSlugMu.RLock()
	cached := officialSlugCache
	officialSlugMu.RUnlock()
	if len(cached) > 0 {
		return cached
	}
	if slugs := loadCodexOfficialSlugs(); len(slugs) > 0 {
		setOfficialSlugCache(slugs)
		officialSlugMu.RLock()
		defer officialSlugMu.RUnlock()
		return officialSlugCache
	}
	entries, err := codexOfficialEntries()
	if err != nil {
		logHub("官方 slug 集合缺失且重算失败：%v", err)
		return nil
	}
	setOfficialSlugCache(codexOfficialSlugList(entries))
	officialSlugMu.RLock()
	defer officialSlugMu.RUnlock()
	return officialSlugCache
}
