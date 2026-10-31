package main

// 模型目录探测（2026-10-13，按用户反馈修正）：一键检查各服务商是否上新模型。
//
// 倍率/免费标注【只用有依据的数据】，不再用关键词启发式瞎猜（此前把
// deepseek-v4-flash、minimax-m3-pay 之类标成"免费"是错误的）：
//   - dom/intl：静态倍率表 model_ratios.go（2026-10 公开资料 + 实测），
//     0 = 免费（Hy3、DS-V4.1-Flash、Hy4-preview）；没进表的模型不标注；
//   - qoder：官方目录自带的 IsFree / price_factor（qfmodel 这类内部 key
//     一律展示官方 DisplayName，避免"fast 是哪家"的困惑）；
//   - trae / zcode：没有公开的每模型倍率（Trae 按次扣积分），一律不标注。
//
// 「新模型」判定：动态目录线路（qoder/zcode）与内置目录做差集。

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type modelProbeItem struct {
	ID       string         `json:"id"`
	Display  string         `json:"display,omitempty"` // 官方显示名（qoder 内部 key 的翻译）
	Ratio    string         `json:"ratio,omitempty"`   // "免费" / "0.06x"；空 = 无公开倍率数据
	Meta     map[string]any `json:"meta,omitempty"`    // 上下文/输入上限/思考档位
	Free     bool           `json:"free"`
	New      bool           `json:"new"`
}

type modelProbeLine struct {
	Line    string           `json:"line"`
	Up      bool             `json:"up"`
	Note    string           `json:"note,omitempty"`
	Models  []modelProbeItem `json:"models"`
	NewCnt  int              `json:"new_count"`
	FreeCnt int              `json:"free_count"`
}

// sortModelsFreeFirst 有倍率的按倍率升序（免费最前），无倍率的排后面按 id 排序。
func sortModelsFreeFirst(items []modelProbeItem) {
	sort.Slice(items, func(i, j int) bool {
		fi, fj := items[i].Free, items[j].Free
		if fi != fj {
			return fi
		}
		ri, rj := ratioValue(items[i].Ratio), ratioValue(items[j].Ratio)
		if ri >= 0 && rj >= 0 && ri != rj {
			return ri < rj
		}
		if (ri >= 0) != (rj >= 0) {
			return ri >= 0
		}
		return items[i].ID < items[j].ID
	})
}

// ratioValue "0.06x"→0.06、"免费"→0、无数据→-1。
func ratioValue(label string) float64 {
	if label == "" {
		return -1
	}
	if label == "免费" {
		return 0
	}
	if v, err := strconv.ParseFloat(strings.TrimSuffix(label, "x"), 64); err == nil {
		return v
	}
	return -1
}

func summarize(line *modelProbeLine) {
	sortModelsFreeFirst(line.Models)
	for _, m := range line.Models {
		if m.New {
			line.NewCnt++
		}
		if m.Free {
			line.FreeCnt++
		}
	}
}

// fetchOpenAIModelList 尽力而为地拿一个 OpenAI 形状的模型列表。
// 依次尝试给定路径，全部失败/解析失败返回 nil。
func fetchOpenAIModelList(headers map[string]string, backend string, paths []string) []string {
	client := backendClient(8 * time.Second) // 打的是上游域名，必须走带死代理回落的 client
	for _, p := range paths {
		req, err := http.NewRequest("GET", backend+p, nil)
		if err != nil {
			continue
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &out) == nil && len(out.Data) > 0 {
			ids := make([]string, 0, len(out.Data))
			for _, m := range out.Data {
				if m.ID != "" {
					ids = append(ids, m.ID)
				}
			}
			if len(ids) > 0 {
				return ids
			}
		}
	}
	return nil
}

// domIntlItem 组一个 dom/intl 模型条目：倍率按 用户覆盖 > 静态表；元数据按实测表。
func (h *Hub) domIntlItem(line, id string) modelProbeItem {
	item := modelProbeItem{ID: id}
	if r, ok := h.modelRatio(line, id); ok {
		item.Ratio = ratioLabel(r)
		item.Free = r == 0
	}
	item.Meta = metaJSON(h.modelMeta(line, id))
	return item
}

// probeLine 探测一条线路。独立函数便于并发与测试。
func probeLine(h *Hub, key string) modelProbeLine {
	line := modelProbeLine{Line: key}
	switch key {
	case "dom", "intl":
		var p *Proxy
		var dir string
		if key == "dom" {
			h.mu.Lock()
			dir = domCredDir(h.domCred)
			h.mu.Unlock()
			p = NewDomProxyDir(dir)
		} else {
			h.mu.Lock()
			cred := h.intlCred
			h.mu.Unlock()
			p = NewIntlProxy(credDirs()[cred])
		}
		line.Up = true
		known := map[string]bool{}
		for _, m := range p.Models {
			known[m] = true
		}
		// 上游目录：GET {Backend}/v3/config（2026-10-16 起，见 wb_config.go）。
		// 以前这里试 /v1/models、/v2/models 两个路径都探不到，于是 note 写着
		// 「上游无公开模型目录接口」——那是错的，接口有，只是不在那两个路径上，
		// 而且要 WorkBuddy/CodeBuddy 形态的 UA。改过来之后倍率和上下文不再是手工表。
		var upstream []string
		cat := h.refreshWBCatalog(key, p, forceWBDirectory)
		if cat == nil {
			cat = wbCatalogGet(key) // 抓不动（线路没起/没凭据）就用快照，过期也比没有强
		}
		if cat != nil {
			line.Note = "倍率/上下文来自上游 /v3/config（实时目录）；抓取失败时回落手工表 model_ratios.go"
			seen := map[string]bool{}
			for _, e := range cat.Entries {
				if !e.Chat || seen[e.ID] {
					continue
				}
				seen[e.ID] = true
				upstream = append(upstream, e.ID)
				it := h.domIntlItem(key, e.ID)
				if e.Name != "" {
					it.Display = e.Name
				}
				if e.Ratio != "" {
					it.Ratio = e.Ratio // 上游口径优先，手工表只兜底
					it.Free = e.Ratio == "免费"
				}
				if e.Context > 0 {
					if it.Meta == nil {
						it.Meta = map[string]any{}
					}
					it.Meta["context_window"] = e.Context
					if e.Default > 0 {
						it.Meta["context_default"] = e.Default
					}
				}
				it.New = !known[e.ID]
				line.Models = append(line.Models, it)
			}
			for _, m := range p.Models {
				if !seen[m] {
					line.Models = append(line.Models, h.domIntlItem(key, m))
				}
			}
		} else if path := p.AuthFilePath(); path != "" && fileExists(path) {
			// 连快照都没有（首次运行且抓取失败）：退回原有尽力而为的探测。
			line.Note = wbCatalogFallbackNote(key)
			cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
			if headers, err := cm.Headers(); err == nil {
				upstream = fetchOpenAIModelList(headers, p.Backend, []string{"/v1/models", "/v2/models"})
			}
			if len(upstream) > 0 {
				seen := map[string]bool{}
				for _, id := range upstream {
					seen[id] = true
					it := h.domIntlItem(key, id)
					it.New = !known[id]
					line.Models = append(line.Models, it)
				}
				for _, m := range p.Models {
					if !seen[m] {
						line.Models = append(line.Models, h.domIntlItem(key, m))
					}
				}
			} else {
				for _, m := range p.Models {
					line.Models = append(line.Models, h.domIntlItem(key, m))
				}
			}
		} else {
			line.Note = wbCatalogFallbackNote(key)
			for _, m := range p.Models {
				line.Models = append(line.Models, h.domIntlItem(key, m))
			}
		}
	case "qoder":
		h.mu.Lock()
		p := h.qoder
		h.mu.Unlock()
		if p == nil || !p.Running() {
			line.Note = "线路未启动，仅列出内置目录"
		} else {
			line.Up = true
			line.Note = "倍率/免费来自 Qoder 官方目录（实时刷新）"
			known := map[string]bool{}
			for _, e := range defaultQoderCatalog() {
				known[e.Key] = true
			}
			for _, e := range p.catalogSnapshot() {
				it := modelProbeItem{ID: e.Key, Display: e.DisplayName}
				if e.IsFree {
					it.Free = true
					it.Ratio = "免费"
				} else if e.PriceFactor > 0 {
					it.Ratio = ratioLabel(e.PriceFactor)
				}
				it.Meta = metaJSON(h.modelMeta("qoder", e.Key))
				it.New = !known[e.Key]
				line.Models = append(line.Models, it)
			}
		}
		if len(line.Models) == 0 {
			for _, e := range defaultQoderCatalog() {
				it := modelProbeItem{ID: e.Key, Display: e.DisplayName}
				if e.IsFree {
					it.Free = true
					it.Ratio = "免费"
				} else if e.PriceFactor > 0 {
					it.Ratio = ratioLabel(e.PriceFactor)
				}
				line.Models = append(line.Models, it)
			}
		}
	case "zcode":
		h.mu.Lock()
		p := h.zcode
		h.mu.Unlock()
		line.Up = p != nil && p.Running()
		line.Note = "无公开每模型倍率，不标注；线路未启动时仅列出内置目录"
		known := map[string]bool{}
		for _, e := range zcodeDefaultCatalog() {
			known[e.Key] = true
		}
		if p != nil && p.Running() {
			for _, e := range p.catalogSnapshot() {
				line.Models = append(line.Models, modelProbeItem{
					ID: e.Key, New: !known[e.Key], Meta: metaJSON(h.modelMeta("zcode", e.Key)),
				})
			}
		}
		if len(line.Models) == 0 {
			for _, e := range zcodeDefaultCatalog() {
				line.Models = append(line.Models, modelProbeItem{ID: e.Key})
			}
		}
	case "trae":
		line.Up = true
		line.Note = "按次扣积分、无公开每模型倍率，不标注免费/倍率"
		for _, m := range TraeModels {
			line.Models = append(line.Models, modelProbeItem{ID: m})
		}
	case "mimo":
		line.Note = "目录固定"
	case "oc":
		// OpenCode 免费档（2026-10-15）：清单是静态表（上游 /v1/models 不返回 limit），
		// 全部 0 计费，元数据带上下文与输出上限。
		line.Up = true
		line.Note = "OpenCode 匿名 public 凭据，全部免费；上下文见元数据"
		for _, m := range opencodeFreeModels {
			line.Models = append(line.Models, modelProbeItem{
				ID: m.ID, Free: true, Ratio: "免费", Meta: metaJSON(h.modelMeta("oc", m.ID)),
			})
		}
	}
	summarize(&line)
	return line
}

var modelProbeLines = []string{"dom", "intl", "qoder", "trae", "zcode", "oc"}

// probeAllModels 并发探测全部线路。
func probeAllModels(h *Hub) []modelProbeLine {
	out := make([]modelProbeLine, len(modelProbeLines))
	var wg sync.WaitGroup
	for i, key := range modelProbeLines {
		wg.Add(1)
		go func(i int, key string) {
			defer recoverGoroutine("model_probe.go:328")
			defer wg.Done()
			out[i] = probeLine(h, key)
		}(i, key)
	}
	wg.Wait()
	return out
}
