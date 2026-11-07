package main

// 探测并更新 = 更新「线路模型库」，绝不动用户的「目录模型」（2026-11-07 用户纠正）。
//
// 用户原话：
//
//	「我想要的探测并更新，是更新数据库里面的模型并不是更新我
//	 【目录模型（客户端可见列表）】这个里面的，聚合线路中的模型我自己手动添加就行」
//
// 上一版（d4e1a06）把官方清单整组镜像进 agg_models.json，冲掉了用户手动维护
// 的目录 —— 那是会错意。这一版的语义：
//
//   - qoder：抓官方 /algo/api/v2/model/list → 热替换运行中线路的目录与别名表
//     （setCatalog），并落盘 qoder_catalog.json（重启后优先于内置快照）。
//   - dom/intl：探测本身会强制重抓 /v3/config 并落盘 wb_catalog_*.json
//     （refreshWBCatalog → wbCatalogPut），这里只负责对比新旧、出回执。
//   - trae/oc/teleagent：清单在代码里维护，没有可更新的库，只报免费项。
//   - agg_models.json（目录模型）一个字节都不动 —— 用户自己手动添加。
//
// 护栏：抓取失败/空清单 → 该线路模型库原样不动，写进 Skipped 告知用户。
// 回执：Updated（真更新了的线路）/ Added / Removed（模型库的新增与下架）/
// Free（本次免费模型，只报有官方依据的）/ Skipped。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// probeSyncOptions 同步选项。空 body / 省略 = 所有线路。
type probeSyncOptions struct {
	Line string `json:"line"` // 空 = 所有线路
}

// probeSyncSummary 给控制台的回执。字符串都是「线路.模型」，可直接展示。
type probeSyncSummary struct {
	Updated []string `json:"updated"` // 模型库真被更新的线路
	Added   []string `json:"added"`   // 模型库新增（官方上新）
	Removed []string `json:"removed"` // 模型库移除（官方下架）
	Free    []string `json:"free"`    // 本次免费模型（只报有官方依据的）
	Skipped []string `json:"skipped"` // 抓取失败/无权威清单，模型库没动的线路
}

// probeSyncDisplayName 回执里 qoder 用官方显示名（QoDuck），其余用 id。
func probeSyncDisplayName(l modelProbeLine, m modelProbeItem) string {
	if l.Line == "qoder" && m.Display != "" {
		return m.Display
	}
	return m.ID
}

// syncModelCatalogs 更新各线路模型库并返回回执 + 探测明细。
// 绝不读写 agg_models.json / aggList —— 目录模型由用户手动维护。
func syncModelCatalogs(h *Hub, opts probeSyncOptions) (probeSyncSummary, []modelProbeLine) {
	res := probeSyncSummary{
		Updated: []string{}, Added: []string{}, Removed: []string{},
		Free: []string{}, Skipped: []string{},
	}
	want := func(line string) bool { return opts.Line == "" || opts.Line == line }

	// ---- qoder：抓官方清单 → 热替换 + 落盘 ----
	if want("qoder") && !lineHidden("qoder") {
		old := map[string]string{} // key -> 显示名（对比基线）
		if p := h.qoderForAgg(); p != nil {
			for _, e := range p.catalogSnapshot() {
				old[e.Key] = e.DisplayName
			}
		} else if disk := loadQoderCatalogDB(); len(disk) > 0 {
			for _, e := range disk {
				old[e.Key] = e.DisplayName
			}
		} else {
			for _, e := range defaultQoderCatalog() {
				old[e.Key] = e.DisplayName
			}
		}
		entries, err := probeQoderFetch()
		if err != nil || len(entries) == 0 {
			res.Skipped = append(res.Skipped, "qoder")
			if err != nil {
				logHub("qoder 模型库更新失败（沿用现有目录）：%v", err)
			}
		} else {
			if p := h.qoderForAgg(); p != nil {
				p.setCatalog(entries)
			}
			if err := saveQoderCatalogDB(entries); err != nil {
				logHub("qoder 模型库落盘失败：%v", err)
			}
			res.Updated = append(res.Updated, "qoder")
			fresh := make(map[string]bool, len(entries))
			for _, e := range entries {
				fresh[e.Key] = true
				if _, ok := old[e.Key]; !ok {
					res.Added = append(res.Added, "qoder."+e.DisplayName)
				}
			}
			for key, display := range old {
				if !fresh[key] {
					res.Removed = append(res.Removed, "qoder."+display)
				}
			}
		}
	}

	// ---- dom/intl：先记旧清单，探测本身会重抓并落盘 wb_catalog_*.json ----
	oldWB := map[string]map[string]bool{}
	for _, line := range []string{"dom", "intl"} {
		if !want(line) || lineHidden(line) {
			continue
		}
		ids := map[string]bool{}
		if c := wbCatalogGet(line); c != nil {
			for _, e := range c.Entries {
				if e.Chat {
					ids[e.ID] = true
				}
			}
		}
		oldWB[line] = ids
	}

	lines := probeAllModels(h)

	for _, l := range lines {
		if !want(l.Line) || lineHidden(l.Line) {
			continue
		}
		if l.Line == "dom" || l.Line == "intl" {
			if !l.Authoritative {
				res.Skipped = append(res.Skipped, l.Line)
			} else {
				res.Updated = append(res.Updated, l.Line)
				fresh := map[string]bool{}
				for _, m := range l.Models {
					if !m.Official || m.ID == "" {
						continue
					}
					fresh[m.ID] = true
					if !oldWB[l.Line][m.ID] {
						res.Added = append(res.Added, l.Line+"."+m.ID)
					}
				}
				for id := range oldWB[l.Line] {
					if !fresh[id] {
						res.Removed = append(res.Removed, l.Line+"."+id)
					}
				}
			}
		}
		// 免费汇总：只报有官方依据的（Official），静态兜底表里的旧「免费」不算。
		for _, m := range l.Models {
			if !m.Official || !m.Free || m.ID == "" {
				continue
			}
			name := l.Line + "." + probeSyncDisplayName(l, m)
			if !containsString(res.Free, name) {
				res.Free = append(res.Free, name)
			}
		}
	}

	sort.Strings(res.Updated)
	sort.Strings(res.Added)
	sort.Strings(res.Removed)
	sort.Strings(res.Free)
	sort.Strings(res.Skipped)
	return res, lines
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------- qoder 模型库落盘（qoder_catalog.json） ----------
//
// 「探测并更新」抓到的官方清单写在这里；线路启动时优先用它（比内置快照新），
// 随后仍会异步向官方刷新。文件损坏/为空一律当没有，回落内置快照。

func qoderCatalogDBPath() string { return dataPath("qoder_catalog.json") }

func saveQoderCatalogDB(entries []qoderModelEntry) error {
	if len(entries) == 0 {
		return nil // 空清单不落盘，免得把好的覆盖成空
	}
	b, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		return err
	}
	tmp := qoderCatalogDBPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, qoderCatalogDBPath())
}

func loadQoderCatalogDB() []qoderModelEntry {
	b, err := os.ReadFile(qoderCatalogDBPath())
	if err != nil {
		return nil
	}
	var out []qoderModelEntry
	if json.Unmarshal(b, &out) != nil || len(out) == 0 {
		return nil
	}
	return out
}

// apiModelsProbeSync 一次请求完成「探测 + 更新线路模型库 + 免费汇总」。
// 与 /api/models/probe（只读）分开；两者都不碰用户的目录模型。
func (h *Hub) apiModelsProbeSync(w http.ResponseWriter, r *http.Request) {
	var opts probeSyncOptions
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&opts); err != nil {
		opts = probeSyncOptions{} // 空 body / 非 JSON 都按默认值
	}
	opts.Line = strings.TrimSpace(opts.Line)
	res, lines := syncModelCatalogs(h, opts)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "lines": lines, "sync": res,
		"msg": fmt.Sprintf("模型库已更新：%d 条线路刷新、新增 %d、下架 %d、免费 %d（你的目录模型未改动）",
			len(res.Updated), len(res.Added), len(res.Removed), len(res.Free)),
	})
}
