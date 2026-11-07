package main

// 探测结果一键同步（2026-11-07 用户要求）：
//
//	「探测新模型，如果探测了就直接替换就行啊，然后把免费告诉我就行」
//	「跟着官方探测的模型来就行，如果官网app里面删除了某个模型，那你也得删除掉，
//	  就可以理解为一次模型更新」
//
// 语义（2026-11-07 用户补充后收紧）：
//
//	「跟着官方探测的模型来就行，如果官网 app 里面删除了某个模型，
//	  那你也得删除掉，就可以理解为一次模型更新」
//
// 即：点一次探测 = 一次模型更新。对每条拿到权威官方清单的线路，
// 目录模型里的该线路条目被整组替换为官方清单（一个不多一个不少）：
//   - 官方新增的 → 加进来（QoDuck 这类免费新模型自然在内）
//   - 官方下架的 → 删掉
//   - 用户自己加的、官方清单里没有的 → 也删掉（严格镜像）
//   - 回执：把「本次有哪些免费模型」单独汇总给控制台显示
//
// 护栏（删错了比少加一个麻烦得多）：
//   - Authoritative=false（线路没起、目录刷新失败、只有离线内置快照）→ 只增不删
//   - 探测结果为空的线路 → 整条跳过，绝不拿空清单清空目录
//   - 隐藏线路 → 不参与同步
//   - 任何删除发生前，先把当前 agg_models.json 备份成 .bak-<时间戳>（数据根内）
//
// 两种模式：
//   - 默认 auto：严格镜像官方清单（用户要的「一次模型更新」）
//   - supplement（界面「只补充不删」）：只补新/免费模型，保留自选清单
//
// 注：DOM/INTL 的 /v3/config 与 qoder 的 /algo/api/v2/model/list 都是官方
// 实时目录，探测时会主动重抓；抓不到才会落到离线快照（Authoritative=false）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// probeSyncOptions 同步选项。空 body / 全部省略 = 所有线路 / auto / 允许删除。
type probeSyncOptions struct {
	Line  string `json:"line"`  // 空 = 所有线路
	Mode  string `json:"mode"`  // auto(默认，严格镜像) / supplement(只补不删) / free / all
	Purge *bool  `json:"purge"` // nil 或 true = 移除官方已下架的；false = 只增不删
}

// probeSyncSummary 给控制台的回执。字符串都是「线路.模型」，可直接展示。
type probeSyncSummary struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Free      []string `json:"free"`
	Lines     []string `json:"lines"`
	Skipped   []string `json:"skipped"` // 没有权威清单、只增不删的线路
	Backup    string   `json:"backup,omitempty"`
	Unchanged int      `json:"unchanged"`
}

// probeSyncBackend 探测条目 → 聚合目录里的 backend。
//
// Qoder 的官方内部 key（qfmodel / lmodel）和聚合目录、/v1/models 的 id、DSH
// profile 里引用的名字都不是一套：那边一直用官方 DisplayName
// （qoder.Qwen3.8-Flash）。若这里写 key，会把用户已有的 slug 全换掉，DSH 里
// 正在用的模型也跟着失效。别名表把 DisplayName 映射回 key，所以写 DisplayName
// 完全能路由，且 QoDuck → "QoDuck" 也走同一套。
func probeSyncBackend(l modelProbeLine, m modelProbeItem) string {
	if l.Line == "qoder" && m.Display != "" {
		return m.Display
	}
	return m.ID
}

// probeSyncAliases 判「旧条目是否还在官方清单」时认可的写法。
// qoder 存量条目既有 DisplayName（qoder.Qwen3.8-Flash）也可能有内部 key
// （qoder.qfmodel），两种都不该被当成下架删掉；官方改名后遗留的历史别名
// （如 Kimi-K3 → kmodel_latest）同理，仍能被别名表路由，不算下架。
func probeSyncAliases(l modelProbeLine, m modelProbeItem) []string {
	out := []string{m.ID}
	if l.Line == "qoder" {
		if m.Display != "" && m.Display != m.ID {
			out = append(out, m.Display)
		}
		for alias, key := range buildQoderAliases(defaultQoderCatalog()) {
			if key == m.ID {
				out = append(out, alias)
			}
		}
	}
	return out
}

// probeSyncKeeps 旧条目的 backend 是否还在官方清单里。
// 别名表里的 key 是归一化写法（kimi-k3），backend 是原样（Kimi-K3），两种都查。
func probeSyncKeeps(set map[string]bool, backend string) bool {
	return set[backend] || set[qoderNormalizeAlias(backend)]
}

// probeSyncMatch 某条探测结果是否要「新增」进目录。
//   - auto（默认）：整组镜像官方清单，Official 的全部加进来（0.1x 也在内）
//   - supplement：只补新/免费，且永不删除
//   - free：只免费
//   - all：探测到的全部（含本地兜底；显式要求时才这么宽）
//
// 非 Official 的条目是本地手工兜底，不该冒充「官方上新」写进目录。
func probeSyncMatch(it modelProbeItem, mode string) bool {
	if it.ID == "" {
		return false
	}
	switch mode {
	case "all":
		return true
	case "free":
		return it.Official && it.Free
	case "supplement":
		return it.Official && (it.New || it.Free)
	default: // auto
		return it.Official
	}
}

// backupAggModels 删除发生前备份当前白名单。失败只记日志、不阻断同步
// （备份是保险，不是一致性前提；目录本身仍是原子可重建的）。
func backupAggModels() string {
	src := dataPath("agg_models.json")
	b, err := os.ReadFile(src)
	if err != nil {
		return ""
	}
	name := "agg_models.json.bak-" + time.Now().Format("20060102-150405")
	dst := dataPath(name)
	if err := os.WriteFile(dst, b, 0600); err != nil {
		logHub("模型目录备份失败：%v", err)
		return ""
	}
	logHub("模型目录更新前已备份：%s", name)
	return name
}

// syncProbeModels 把探测结果同步进目录模型，返回可直接展示的回执。
// 只在确有增删时写盘，并异步刷新桌面版目录。
func syncProbeModels(lines []modelProbeLine, opts probeSyncOptions) probeSyncSummary {
	res := probeSyncSummary{
		Added: []string{}, Removed: []string{}, Free: []string{},
		Lines: []string{}, Skipped: []string{},
	}
	mode := opts.Mode
	if mode == "" {
		mode = "auto"
	}
	purge := opts.Purge == nil || *opts.Purge

	// 免费汇总与 mode 无关：用户要的就是「把免费告诉我就行」。
	// 隐藏线路不报（用户已主动藏起来，别再拿它刷屏）。
	for _, l := range lines {
		if opts.Line != "" && l.Line != opts.Line {
			continue
		}
		if !aggSelectableLine(l.Line) {
			continue
		}
		for _, m := range l.Models {
			if !m.Free || m.ID == "" {
				continue
			}
			name := l.Line + "." + probeSyncBackend(l, m)
			if m.Display != "" && m.Display != m.ID && name != l.Line+"."+m.Display {
				name += "（" + m.Display + "）"
			}
			if !containsString(res.Free, name) {
				res.Free = append(res.Free, name)
			}
		}
	}

	// addSet  = 本模式要新增的 backend
	// keepSet = 官方清单里出现过的所有写法（purge 时用它判断旧条目是否该留）
	addSet := map[string]map[string]bool{}
	keepSet := map[string]map[string]bool{}
	order := make([]string, 0, len(lines))
	for _, l := range lines {
		if opts.Line != "" && l.Line != opts.Line {
			continue
		}
		if !aggSelectableLine(l.Line) {
			continue
		}
		if len(l.Models) == 0 {
			// 空清单绝不参与替换；明确写进回执，别让用户以为同步过了。
			res.Skipped = append(res.Skipped, l.Line)
			continue
		}
		if _, ok := addSet[l.Line]; !ok {
			order = append(order, l.Line)
			addSet[l.Line] = map[string]bool{}
		}
		// 只有权威清单才允许删除；非权威只增不删，并写进回执告知用户。
		canPurge := purge && mode != "supplement" && l.Authoritative
		if purge && mode != "supplement" && !l.Authoritative {
			res.Skipped = append(res.Skipped, l.Line)
		}
		if canPurge && keepSet[l.Line] == nil {
			keepSet[l.Line] = map[string]bool{}
		}
		for _, m := range l.Models {
			if m.ID == "" {
				continue
			}
			if canPurge && m.Official {
				for _, a := range probeSyncAliases(l, m) {
					keepSet[l.Line][a] = true
				}
			}
			if probeSyncMatch(m, mode) {
				addSet[l.Line][probeSyncBackend(l, m)] = true
			}
		}
	}

	aggMu.Lock()
	existing := make(map[string]bool, len(aggList))
	for _, m := range aggList {
		existing[m.Slug] = true
	}
	kept := make([]aggModel, 0, len(aggList)+64)
	for _, m := range aggList {
		if set := keepSet[m.Line]; set != nil && !probeSyncKeeps(set, m.Backend) {
			res.Removed = append(res.Removed, m.Slug)
			continue
		}
		kept = append(kept, m)
	}
	for _, line := range order {
		backends := make([]string, 0, len(addSet[line]))
		for b := range addSet[line] {
			backends = append(backends, b)
		}
		sort.Strings(backends)
		for _, backend := range backends {
			slug := line + "." + backend
			if existing[slug] {
				continue
			}
			kept = append(kept, aggModel{
				Line: line, Slug: slug, Backend: backend,
				Display: aggDisplayName(line, backend),
			})
			res.Added = append(res.Added, slug)
			res.Lines = append(res.Lines, line)
		}
	}
	changed := len(res.Added) > 0 || len(res.Removed) > 0
	if changed {
		if len(res.Removed) > 0 {
			res.Backup = backupAggModels()
		}
		aggList = kept
		aggSaveLocked() // 持锁写盘；codexCatalogRefresh 必须在锁外（见 aggAdd 注释）
	}
	unchanged := len(kept) - len(res.Added)
	aggMu.Unlock()

	if changed {
		codexCatalogRefresh()
	}
	sort.Strings(res.Added)
	sort.Strings(res.Removed)
	sort.Strings(res.Free)
	sort.Strings(res.Lines)
	sort.Strings(res.Skipped)
	res.Lines = uniqueStrings(res.Lines)
	res.Skipped = uniqueStrings(res.Skipped)
	res.Unchanged = unchanged
	return res
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

// apiModelsProbeSync 一次请求完成「探测 + 自动同步 + 免费汇总」。
// 与 /api/models/probe（只读）分开，避免普通查看也改目录。
func (h *Hub) apiModelsProbeSync(w http.ResponseWriter, r *http.Request) {
	var opts probeSyncOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		opts = probeSyncOptions{} // 空 body / 非 JSON 都按默认值
	}
	opts.Line = strings.TrimSpace(opts.Line)
	lines := probeAllModels(h)
	res := syncProbeModels(lines, opts)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "lines": lines, "sync": res,
		"msg": fmt.Sprintf("模型更新：新增 %d、移除 %d、免费 %d", len(res.Added), len(res.Removed), len(res.Free)),
	})
}
