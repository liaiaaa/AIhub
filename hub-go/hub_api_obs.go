package main

// 观测类只读接口（2026-10-31 UI 第 1/4/5 项的后端部分）。
//
// 背景：控制台此前拿不到两类最关键的排查信息，只能开终端：
//  1. 限流台账（rate_limit.go 里早就有完整记录：哪个账号/模型在冷却、到几点、
//     命中次数、上游业务码），但没有任何 API 暴露 → 界面上只看到失败，
//     看不出「正在冷却中，还有 8 分钟」；
//  2. 线路日志（converter-*.log 与 hub.log）只能 grep 文件，
//     要定位一条线路最近一次失败得自己翻日志。
//
// 这三个接口全是只读：不写台账、不删文件、不改任何运行时状态。
// 一律走白名单，不接受调用方传路径（避免 ../ 穿越）。

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// logTailMaxLines 单次最多返回的日志行数（防止一眼拉爆整个页面）。
const logTailMaxLines = 500

// logTailMaxBytes 读尾部时最多读这么多字节（够覆盖 500 行常规日志）。
const logTailMaxBytes = 256 << 10

// obsLogFiles 允许查看的日志：hub 主日志 + 各线路日志（来自 lineSpecs）。
// 白名单是唯一入口，调用方传别的名字一律 400。
func obsLogFiles() map[string]string {
	m := map[string]string{"hub": "hub.log"}
	for _, s := range lineSpecs {
		m[s.Key] = s.LogFile
	}
	return m
}

// apiLimits 返回当前限流/冷却台账（只读快照）。
//
// 不返回 UID：那是上游账号标识，看板上用不上，少暴露一个字段就少一份风险。
func (h *Hub) apiLimits(w http.ResponseWriter, r *http.Request) {
	snap := rateLimitSnapshot()
	out := make([]map[string]any, 0, len(snap))
	for _, e := range snap {
		item := map[string]any{
			"name":   e.Name,
			"kind":   e.Kind,
			"code":   e.Code,
			"reason": e.Reason,
			"hits":   e.Hits,
			"until":  e.Until.Format("2006-01-02 15:04:05"),
			"remain": rateLimitRemain(e.Until),
		}
		if e.Model != "" {
			item["model"] = e.Model
		}
		// 上游给没给恢复时刻，界面上要区别对待：
		// 给了才能说「预计 X 恢复」，没给只能说到「本地兜底闸门」为止。
		if !e.ResetAt.IsZero() {
			item["reset_at"] = e.ResetAt.Format("2006-01-02 15:04:05")
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "limits": out, "count": len(out)})
}

// apiLogsTail 返回某个日志文件的尾部若干行。
// 查询参数：file（白名单，默认 hub）、lines（默认 80，上限 500）。
func (h *Hub) apiLogsTail(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("file")
	if key == "" {
		key = "hub"
	}
	files := obsLogFiles()
	name, ok := files[key]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "msg": "未知日志：" + key + "（可选：hub 或线路名）"})
		return
	}
	lines := 80
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	if lines > logTailMaxLines {
		lines = logTailMaxLines
	}

	path := dataPath(name)
	tail, err := tailLines(path, lines)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "file": name, "path": path, "msg": "读不到日志：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "file": name, "path": path, "lines": len(tail), "tail": tail})
}

// apiLogsSizes 返回各日志的体积与最后修改时间（设置页维护区用）。
func (h *Hub) apiLogsSizes(w http.ResponseWriter, r *http.Request) {
	files := obsLogFiles()
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	// 固定顺序：hub 在最前，其余按 lineSpecs 顺序
	out := make([]map[string]any, 0, len(files))
	add := func(key, name string) {
		path := dataPath(name)
		item := map[string]any{"key": key, "file": name, "path": path}
		if st, err := os.Stat(path); err == nil {
			item["size"] = st.Size()
			item["size_text"] = humanBytes(st.Size())
			item["mod_time"] = st.ModTime().Format("2006-01-02 15:04:05")
		} else {
			item["size"] = 0
			item["size_text"] = "—"
		}
		out = append(out, item)
	}
	add("hub", files["hub"])
	for _, s := range lineSpecs {
		add(s.Key, s.LogFile)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "root": hubRoot, "logs": out, "dumps": countDumpFiles()})
}

// countDumpFiles 统计数据根里的失败样本 dump（dom-dump-*.json 等）。
func countDumpFiles() int {
	matches, err := filepath.Glob(filepath.Join(hubRoot, "*-dump-*.json"))
	if err != nil {
		return 0
	}
	return len(matches)
}

// humanBytes 把字节数转成好读的字符串（B/KB/MB/GB）。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + []string{"KB", "MB", "GB", "TB"}[exp]
}

// tailLines 读文件最后 n 行。文件不存在时返回错误（由调用方转成给用户看的提示）。
//
// 只读尾部 256KB，不整文件载入：hub.log 与 converter-*.log 会一直是追写状态，
// 整文件读取在日志大到几十 MB 时会把接口拖慢。
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	start := size - logTailMaxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) > 0 {
		// 读到 EOF 是正常情况（文件在读取过程中又被追写）
		if !strings.Contains(err.Error(), "EOF") {
			return nil, err
		}
	}
	text := strings.TrimRight(string(buf), "\n")
	if text == "" {
		return []string{}, nil
	}
	all := strings.Split(text, "\n")
	if start > 0 && len(all) > 0 {
		all = all[1:] // 首行可能是被切断的半行
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}
