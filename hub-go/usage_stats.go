package main

// Codex 流量在聚合口（:8790）有唯一必经点，token 用量就在那里取样：
// 包一层 ResponseWriter，从透传的 SSE/JSON 里抓 usage，避免侵入六条线路各自的流式实现。
// 落盘成 append-only 的 usage.jsonl，控制台「Token 用量」页按天/按模型聚合。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const usageFile = "usage.jsonl"

var usageKey = []byte(`"usage"`)

type usageRec struct {
	TS      int64  `json:"ts"`
	Date    string `json:"d"`
	Line    string `json:"line"`
	Slug    string `json:"slug"`
	Backend string `json:"backend"`
	Kind    string `json:"kind"`
	Code    int    `json:"code"`
	MS      int64  `json:"ms"`
	In      int    `json:"in"`
	Out     int    `json:"out"`
	Cached  int    `json:"cached"`
	Reason  int    `json:"reason"`
	Total   int    `json:"total"`
}

// usageTap 记录状态码并从响应里捞出最后一个 usage 对象。
// hub 自己的 responsesStream 与 writeJSON 都是整帧一次写出，usage 不会被跨块截断；
// io.Copy 那条路极端情况下可能漏，漏了就记 0，不影响请求本身。
type usageTap struct {
	w     http.ResponseWriter
	code  int
	head  bool
	usage map[string]any
	// tail 上一块数据的尾部，留给下一次 pick 拼帧用。
	// 2026-10-21 开源审查修：原来只在「单次 Write 的 chunk 内」做
	// LastIndex("usage") + 括号配对。SSE 帧跨 32KB 边界时配对必然失败，
	// usage 被静默丢掉（用量统计少记），而正文里出现 "usage" 这个词时还可能
	// 配出垃圾对象。保留尾部后跨块帧也能配出来。
	tail []byte
}

// usageTailKeep 每次保留多少尾部字节参与下一次拼帧。SSE 的 usage 对象通常
// 几十字节；16KB 足够跨两次 32KB 分块，又不会让缓冲无限增长。
const usageTailKeep = 16 << 10

func (t *usageTap) Header() http.Header { return t.w.Header() }

func (t *usageTap) WriteHeader(code int) {
	if !t.head {
		t.code, t.head = code, true
	}
	t.w.WriteHeader(code)
}

func (t *usageTap) Write(p []byte) (int, error) {
	if !t.head {
		t.code, t.head = 200, true
	}
	t.pick(p)
	return t.w.Write(p)
}

func (t *usageTap) Flush() {
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom 保留 io.Copy 的零拷贝路径语义，同时逐块取样（buf 复用，不额外缓冲整个响应）。
func (t *usageTap) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var n int64
	for {
		m, rerr := r.Read(buf)
		if m > 0 {
			if !t.head {
				t.code, t.head = 200, true
			}
			t.pick(buf[:m])
			if _, werr := t.w.Write(buf[:m]); werr != nil {
				return n, werr
			}
			n += int64(m)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return n, nil
			}
			return n, rerr
		}
	}
}

func (t *usageTap) pick(p []byte) {
	// 把上一次的尾部拼在前面，跨 chunk 的帧才配得出来。
	var buf []byte
	if len(t.tail) > 0 {
		buf = make([]byte, 0, len(t.tail)+len(p))
		buf = append(buf, t.tail...)
		buf = append(buf, p...)
	} else {
		buf = p
	}
	if i := bytes.LastIndex(buf, usageKey); i >= 0 {
		if u := usageObjectAt(buf, i); u != nil {
			t.usage = u
		}
	}
	// 保留尾部：从 buf 末尾往前取 usageTailKeep 字节。
	if len(buf) > usageTailKeep {
		t.tail = append(t.tail[:0], buf[len(buf)-usageTailKeep:]...)
	} else {
		t.tail = append(t.tail[:0], buf...)
	}
}

// usageObjectAt 从 "usage" 之后做括号配对，截出对象再解析。
func usageObjectAt(b []byte, i int) map[string]any {
	j := i + len(usageKey)
	for j < len(b) && (b[j] == ' ' || b[j] == ':' || b[j] == '\t') {
		j++
	}
	if j >= len(b) || b[j] != '{' {
		return nil
	}
	depth, inStr, esc := 0, false, false
	for k := j; k < len(b); k++ {
		c := b[k]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var m map[string]any
				dec := json.NewDecoder(bytes.NewReader(b[j : k+1]))
				dec.UseNumber()
				if dec.Decode(&m) == nil {
					return m
				}
				return nil
			}
		}
	}
	return nil
}

func usageInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n := intVal(v, -1); n >= 0 {
				return n
			}
		}
	}
	return 0
}

func usageNested(m map[string]any, obj, field string) int {
	d, ok := m[obj].(map[string]any)
	if !ok {
		return 0
	}
	return usageInt(d, field)
}

// usageFrom 同时认 chat（prompt_tokens…）与 Responses（input_tokens…）两种键名。
func usageFrom(u map[string]any) (in, out, cached, reason, total int) {
	if len(u) == 0 {
		return
	}
	in = usageInt(u, "prompt_tokens", "input_tokens")
	out = usageInt(u, "completion_tokens", "output_tokens")
	total = usageInt(u, "total_tokens")
	if total == 0 {
		total = in + out
	}
	cached = usageNested(u, "prompt_tokens_details", "cached_tokens")
	if cached == 0 {
		cached = usageNested(u, "input_tokens_details", "cached_tokens")
	}
	reason = usageNested(u, "completion_tokens_details", "reasoning_tokens")
	if reason == 0 {
		reason = usageNested(u, "output_tokens_details", "reasoning_tokens")
	}
	return
}

var (
	usageMu sync.Mutex
)

func recordUsage(r usageRec) {
	r.TS = time.Now().Unix()
	r.Date = time.Now().Format("2006-01-02")
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	f, err := os.OpenFile(dataPath(usageFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// recordTap 在聚合口一次请求结束时落一行。
func recordTap(t *usageTap, am aggModel, kind string, ms int64) {
	in, out, cached, reason, total := usageFrom(t.usage)
	code := t.code
	if code == 0 {
		code = 200
	}
	recordUsage(usageRec{
		Line: am.Line, Slug: am.Slug, Backend: am.Backend, Kind: kind,
		Code: code, MS: ms, In: in, Out: out, Cached: cached, Reason: reason, Total: total,
	})
}

type usageAgg struct {
	Reqs   int   `json:"reqs"`
	In     int64 `json:"in"`
	Out    int64 `json:"out"`
	Cached int64 `json:"cached"`
	Reason int64 `json:"reason"`
	Total  int64 `json:"total"`
	Fail   int   `json:"fail"`
}

func (a *usageAgg) add(r usageRec) {
	a.Reqs++
	a.In += int64(r.In)
	a.Out += int64(r.Out)
	a.Cached += int64(r.Cached)
	a.Reason += int64(r.Reason)
	a.Total += int64(r.Total)
	if r.Code < 200 || r.Code >= 400 {
		a.Fail++
	}
}

// 增量解析缓存：文件只在尾部增长，按 size 记住已读偏移，别每次都重扫全文。
var (
	usageCacheMu   sync.Mutex
	usageCacheRows []usageRec
	usageCacheOff  int64
	usageCacheSize int64
)

func usageRows() ([]usageRec, error) {
	path := dataPath(usageFile)
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	usageCacheMu.Lock()
	defer usageCacheMu.Unlock()
	if st.Size() < usageCacheOff { // 文件被清空/轮转
		usageCacheRows, usageCacheOff = nil, 0
	}
	if st.Size() > usageCacheOff {
		f, err := os.Open(path)
		if err != nil {
			return usageCacheRows, err
		}
		defer f.Close()
		if _, err := f.Seek(usageCacheOff, io.SeekStart); err != nil {
			return usageCacheRows, err
		}
		rows, consumed, err := scanUsage(f)
		if err != nil {
			return usageCacheRows, err
		}
		usageCacheRows = append(usageCacheRows, rows...)
		usageCacheOff += consumed // 尾部写了一半的行下次再读，不丢记录
	}
	usageCacheSize = st.Size()
	return usageCacheRows, nil
}

// scanUsage 按行解析 append-only 的 usage.jsonl，返回消费掉的字节数。
// 只消费以换行结尾的完整行：尾部写了一半的行原样留给下次增量读，不丢记录。
func scanUsage(r io.Reader) ([]usageRec, int64, error) {
	br := bufio.NewReader(r)
	out := []usageRec{}
	var consumed int64
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			break
		}
		var rec usageRec
		if json.Unmarshal(line, &rec) == nil {
			out = append(out, rec)
		}
		consumed += int64(len(line))
	}
	return out, consumed, nil
}

// usageReport 按 days（0=全部）聚合出按天/按线路/按模型三份表。
func usageReport(days int) map[string]any {
	rows, err := usageRows()
	if err != nil {
		return map[string]any{"ok": false, "msg": "读取用量失败：" + err.Error()}
	}
	cutoff := ""
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days+1).Format("2006-01-02")
	}
	byDay := map[string]*usageAgg{}
	byLine := map[string]*usageAgg{}
	type mk struct{ line, slug string }
	byModel := map[mk]*usageAgg{}
	span := map[string]bool{}
	var sum usageAgg
	for _, r := range rows {
		if cutoff != "" && r.Date < cutoff {
			continue
		}
		sum.add(r)
		span[r.Date] = true
		if byDay[r.Date] == nil {
			byDay[r.Date] = &usageAgg{}
		}
		byDay[r.Date].add(r)
		if byLine[r.Line] == nil {
			byLine[r.Line] = &usageAgg{}
		}
		byLine[r.Line].add(r)
		k := mk{r.Line, r.Slug}
		if byModel[k] == nil {
			byModel[k] = &usageAgg{}
		}
		byModel[k].add(r)
	}

	dates := make([]string, 0, len(byDay))
	for d := range byDay {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	dayRows := make([]map[string]any, 0, len(dates))
	for _, d := range dates {
		a := byDay[d]
		dayRows = append(dayRows, map[string]any{"date": d, "agg": a})
	}

	lineKeys := make([]string, 0, len(byLine))
	for l := range byLine {
		lineKeys = append(lineKeys, l)
	}
	sort.Strings(lineKeys)
	lineRows := make([]map[string]any, 0, len(lineKeys))
	for _, l := range lineKeys {
		a := byLine[l]
		lineRows = append(lineRows, map[string]any{
			"line": l, "line_cn": aggLineCN[l], "agg": a,
		})
	}

	modelRows := make([]map[string]any, 0, len(byModel))
	for k, a := range byModel {
		modelRows = append(modelRows, map[string]any{
			"line": k.line, "line_cn": aggLineCN[k.line], "slug": k.slug, "agg": a,
		})
	}
	sort.Slice(modelRows, func(i, j int) bool {
		si, _ := modelRows[i]["agg"].(*usageAgg)
		sj, _ := modelRows[j]["agg"].(*usageAgg)
		if si.Total != sj.Total {
			return si.Total > sj.Total
		}
		return modelRows[i]["slug"].(string) < modelRows[j]["slug"].(string)
	})
	if len(modelRows) > 30 {
		modelRows = modelRows[:30]
	}

	first, last := "", ""
	if len(dates) > 0 {
		last, first = dates[0], dates[len(dates)-1]
	}
	return map[string]any{
		"ok":       true,
		"days":     dayRows,
		"lines":    lineRows,
		"models":   modelRows,
		"summary":  sum,
		"from":     first,
		"to":       last,
		"span":     len(span),
		"file_mb":  strconv.FormatFloat(float64(usageCacheSize)/(1<<20), 'f', 2, 64),
		"window":   days,
		"recorded": len(rows),
	}
}


// apiUsage 给控制台返回用量报表，?days=7|14|30|0(全部)。
func (h *Hub) apiUsage(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			days = n
		}
	}
	writeJSON(w, http.StatusOK, usageReport(days))
}
