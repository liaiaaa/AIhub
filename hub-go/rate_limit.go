package main

// 限流台账：记下「哪个账号、哪个模型被限到什么时候」。
//
// 背景（2026-10-12 用户要求）：上游 6004 的文案里带恢复时刻
// （"your usage will reset at 2026-10-12 20:01:25 UTC+8"），但只出现在
// 报错的那一刻——用户记不住，下次遇到又要翻日志。而且实测发现 11140
// （账号级风控）与 6004（限流）**性质不同**，混在一起看会误判。
//
// 设计参考（两个 MIT 开源实现，均 2026-10-12 活跃）：
//   - linguo2625469/workbuddy2api-panel internal/pool/cooldown.go
//   - ardeyouxipianyi/workbuddy2api-hub
// 采纳其中三条关键做法：
//   1) 冷却截止取 min(上游 resetAt, now+上限)——防上游给离谱远期时间把号锁死；
//   2) 区分「账号级」（11140 风控，全模型不可用）与「模型级」（6004 限流，
//      同账号换模型可用）；
//   3) 无 resetAt 时按有界退避兜底，而不是永久拉黑。
//
// 刻意不做的事：**不自动轮换凭证**。轮换会改变请求路由，一旦逻辑有 bug
// 可能把好账号拖下水；当前只做「如实记录 + 展示」，换号由用户决定。

import (
"bytes"
	"encoding/json"
"io"
"net/http"
	"os"
"strings"
	"sync"
	"time"
)

const rateLimitFile = "rate_limit.json"

// rateLimitMaxUntil 冷却上限：即便上游说"明天恢复"，也最多锁这么久。
// 防止上游脏数据/时钟偏移把账号永久拉黑（参考 cooldown.go 的 cappedSoftUntil）。
const rateLimitMaxUntil = 2 * time.Hour

// rateLimitKind 限流类型。
const (
	rlKindModel   = "model"   // 模型级：同账号换模型可用（6004 常见）
	rlKindAccount = "account" // 账号级：该账号全模型不可用（11140 风控）
)

// rateLimitEntry 一次限流记录。
type rateLimitEntry struct {
	UID    string `json:"uid"`
	Name   string `json:"name,omitempty"`  // 昵称，便于辨认
	Kind   string `json:"kind"`            // model / account
	Model  string `json:"model,omitempty"` // 模型级时记录具体模型
	Code   int    `json:"code,omitempty"`  // 上游业务码（6004 / 11140 …）
	Reason string `json:"reason,omitempty"`// 简短原因
	Until  time.Time `json:"until"`        // 冷却截止（已按上限截断）
	// ResetAt 上游**明确给出**的恢复时刻。为零表示上游没给（如 11140 风控
	// 只回「内容未通过安全审核」，不含任何时间）——此时 Until 只是本地兜底，
	// **不能当作"到点就恢复"展示**（2026-10-13 修正：此前会把兜底时间
	// 显示成"恢复 10:24"，是假信息）。
	ResetAt time.Time `json:"reset_at,omitempty"`
	SeenAt  time.Time `json:"seen_at"` // 首次记录时刻
	Hits    int       `json:"hits"`    // 累计命中次数
}

// HasUpstreamReset 上游是否明确给了恢复时刻。
func (e rateLimitEntry) HasUpstreamReset() bool { return !e.ResetAt.IsZero() }

var rateLimitState = struct {
	mu      sync.Mutex
	loaded  bool
	entries map[string]*rateLimitEntry // key = uid + "|" + kind + "|" + model
}{entries: map[string]*rateLimitEntry{}}

func rateLimitPath() string { return dataPath(rateLimitFile) }

func rlKey(uid, kind, model string) string {
	if kind == rlKindAccount {
		return uid + "|account"
	}
	return uid + "|model|" + model
}

func loadRateLimitLocked() {
	if rateLimitState.loaded {
		return
	}
	rateLimitState.loaded = true
	b, err := os.ReadFile(rateLimitPath())
	if err != nil {
		return
	}
	var raw struct {
		Entries map[string]*rateLimitEntry `json:"entries"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Entries == nil {
		return
	}
	now := time.Now()
	for k, e := range raw.Entries {
		if e == nil || !now.Before(e.Until) {
			continue // 已过期的不载入
		}
		rateLimitState.entries[k] = e
	}
}

func saveRateLimitLocked() {
	raw := struct {
		Entries map[string]*rateLimitEntry `json:"entries"`
	}{Entries: rateLimitState.entries}
	b, err := json.MarshalIndent(raw, "", " ")
	if err != nil {
		return
	}
	path := rateLimitPath()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// rateLimitRecord 记一次限流。
//   - kind=account：该账号全模型冷却（11140）
//   - kind=model：仅该模型冷却（6004 常见）
//
// resetAt 为零表示上游没给恢复时刻，此时按有界退避兜底。
func rateLimitRecord(uid, name, kind, model string, code int, reason string, resetAt time.Time) {
	if uid == "" {
		return
	}
	now := time.Now()
	until := now.Add(rateLimitMaxUntil) // 无 resetAt 的兜底
	if !resetAt.IsZero() {
		// 取较早者：既尊重上游，又不被离谱的远期时间锁死
		until = resetAt
		if until.After(now.Add(rateLimitMaxUntil)) {
			until = now.Add(rateLimitMaxUntil)
		}
		if until.Before(now) {
			until = now.Add(time.Minute) // 上游时刻已过：给个短冷却即可
		}
	}

	rateLimitState.mu.Lock()
	defer rateLimitState.mu.Unlock()
	loadRateLimitLocked()

	k := rlKey(uid, kind, model)
	e := rateLimitState.entries[k]
	if e == nil {
		e = &rateLimitEntry{UID: uid, Name: name, Kind: kind, Model: model, SeenAt: now}
		rateLimitState.entries[k] = e
	}
	if name != "" {
		e.Name = name
	}
	e.Code = code
	e.Reason = reason
	e.Until = until
	e.ResetAt = resetAt
	e.Hits++
	saveRateLimitLocked()
}

// rateLimitSnapshot 返回当前仍有效的限流记录（已清过期）。
func rateLimitSnapshot() []rateLimitEntry {
	now := time.Now()
	rateLimitState.mu.Lock()
	defer rateLimitState.mu.Unlock()
	loadRateLimitLocked()

	out := make([]rateLimitEntry, 0, len(rateLimitState.entries))
	dirty := false
	for k, e := range rateLimitState.entries {
		if e == nil || !now.Before(e.Until) {
			delete(rateLimitState.entries, k) // 过期即清
			dirty = true
			continue
		}
		out = append(out, *e)
	}
	if dirty {
		saveRateLimitLocked()
	}
	return out
}

// rateLimitFor 查某个账号（可选指定模型）当前是否被限。
// 账号级冷却对任意模型都生效；模型级只对指定模型生效。
func rateLimitFor(uid, model string) (rateLimitEntry, bool) {
	now := time.Now()
	rateLimitState.mu.Lock()
	defer rateLimitState.mu.Unlock()
	loadRateLimitLocked()

	if e, ok := rateLimitState.entries[rlKey(uid, rlKindAccount, "")]; ok && e != nil && now.Before(e.Until) {
		return *e, true
	}
	if model != "" {
		if e, ok := rateLimitState.entries[rlKey(uid, rlKindModel, model)]; ok && e != nil && now.Before(e.Until) {
			return *e, true
		}
	}
	return rateLimitEntry{}, false
}

// rateLimitClearUID 清掉某账号的全部限流记录（如换凭证后确认可用）。
func rateLimitClearUID(uid string) {
	rateLimitState.mu.Lock()
	defer rateLimitState.mu.Unlock()
	loadRateLimitLocked()
	dirty := false
	for k, e := range rateLimitState.entries {
		if e != nil && e.UID == uid {
			delete(rateLimitState.entries, k)
			dirty = true
		}
	}
	if dirty {
		saveRateLimitLocked()
	}
}

// rateLimitShortUntil 把截止时刻格式化成「20:01」或「10-13 04:00」（跨天时带日期）。
func rateLimitShortUntil(t time.Time) string {
	now := time.Now()
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// rateLimitRemain 剩余时长的中文短文案，如「还剩 42 分钟」。
func rateLimitRemain(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "已恢复"
	}
	if d < time.Minute {
		return "还剩 <1 分钟"
	}
	if d < time.Hour {
		return "还剩 " + itoa(int(d.Minutes())) + " 分钟"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return "还剩 " + itoa(h) + " 小时"
	}
	return "还剩 " + itoa(h) + " 小时 " + itoa(m) + " 分"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------- 国际凭证可用性总览（控制台「哪个账号能用」） ----------

// intlCredStatus 一个国际凭证的可用性快照。
type intlCredStatus struct {
	Name     string  `json:"name"`               // 凭证目录名
	Nick     string  `json:"nick,omitempty"`     // 账号昵称
	UID      string  `json:"uid,omitempty"`
	Remain   float64 `json:"remain"`             // 余额（-1 表示没查到）
	Total    float64 `json:"total"`
	OK       bool    `json:"ok"`                 // 现在能不能用
	State    string  `json:"state"`              // 简短状态：可用 / 限流 / 风控 / 未知
	Until    string  `json:"until,omitempty"`    // 限流截止（如 "20:01"）
	RemainTx string  `json:"remain_txt,omitempty"` // 「还剩 42 分钟」
	// UpstreamReset 上游是否明确给了恢复时刻。false 时 Until 只是本地兜底，
	// 前端**不得**把它展示成"恢复时间"。
	UpstreamReset bool `json:"upstream_reset"`
	Reason   string  `json:"reason,omitempty"`   // 原因（限流/风控）
	Selected bool    `json:"selected"`           // 是否是当前选中
}

// intlCredsStatus 汇总全部国际凭证的可用性：
//   - 额度：直接查上游（并发，各自独立超时）
//   - 限流/风控：查本地台账（rateLimitFor）
//
// 这是 C 方案的核心：让用户一眼看出「哪个账号现在能用」，而不是靠记忆。
// 不做自动轮换——只如实汇报，换号由用户决定。
func intlCredsStatus(h *Hub, force bool) []intlCredStatus {
	h.mu.Lock()
	selected := h.intlCred
	h.mu.Unlock()

	dirs := credDirs()
	names := make([]string, 0, len(dirs))
	for name, dir := range dirs {
		if dir != "" && dirHasCredential(dir) {
			names = append(names, name)
		}
	}
	sortStrings(names)

	out := make([]intlCredStatus, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			out[i] = intlCredStatusOne(name, dirs[name], name == selected)
		}(i, name)
	}
	wg.Wait()
	return out
}

func intlCredStatusOne(name, dir string, selected bool) intlCredStatus {
	st := intlCredStatus{Name: name, Remain: -1, Selected: selected, State: "未知"}

	p := NewIntlProxy(dir)
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		st.State = "无凭据"
		return st
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	if sum, err := cm.Summary(); err == nil {
		st.UID, _ = sum["uid"].(string)
		st.Nick, _ = sum["nickname"].(string)
	}

	// 余额（顺带查，用于判断"是余额没了还是频率限制"）
	if remain, total, ok := intlCreditsRaw(cm, p); ok {
		st.Remain, st.Total = remain, total
	}

	// 先看本地限流台账（比探活快，且能给出「限到几点」）
	if e, limited := rateLimitFor(st.UID, ""); limited {
		st.OK = false
		st.Reason = e.Reason
		st.UpstreamReset = e.HasUpstreamReset()
		if e.Kind == rlKindAccount {
			st.State = "风控"
			// 风控（11140）上游从不给恢复时刻：只报"被拦"，不编时间。
			if st.UpstreamReset {
				st.Until = rateLimitShortUntil(e.Until)
				st.RemainTx = rateLimitRemain(e.Until)
			}
		} else {
			st.State = "限流"
			st.Until = rateLimitShortUntil(e.Until)
			st.RemainTx = rateLimitRemain(e.Until)
		}
		return st
	}

	// 台账没记录 ≠ 能用。**必须真探一次**：额度查得到只说明 billing 通，
	// 与推理通道是两回事（2026-10-12 实测<凭证目录A>：额度 200、userinfo 200、
	// 但 chat 403 11140）。此前这里直接标 ✓ 可用，等于在骗用户。
	probe := intlProbeInfer(cm, p)
	switch probe {
	case probeOK:
		st.OK = true
		st.State = "可用"
	case probeRisk:
		st.OK = false
		st.State = "风控"
		st.Reason = "账号级风控(11140)"
		st.UpstreamReset = false
		// 探活发现的风控也记进台账，后续查询就不必反复探
		rateLimitRecord(st.UID, st.Nick, rlKindAccount, "", 11140, st.Reason, time.Time{})
	case probeRateLimited:
		st.OK = false
		st.State = "限流"
		st.Reason = "用量限流(6004)"
	default:
		st.OK = false
		st.State = "探活失败"
	}
	return st
}

// 探活结果
type probeResult int

const (
	probeOK probeResult = iota
	probeRisk
	probeRateLimited
	probeUnknown
)

// intlProbeInfer 发一个最小推理请求，判断该凭证的**推理通道**是否可用。
//
// 为什么必须探活：额度接口（billing）与推理接口（chat）是两条通道，
// 额度 200 不代表能推理。2026-10-12 实测<凭证目录A>就是这种（额度/userinfo/
// 成长中心全 200，chat 却 403 11140）。只看额度会把坏号标成"可用"。
//
// 成本控制：max_tokens=1、prompt 极短、只读到第一帧就断开——不消耗实质额度
// （实测免费模型本来就不计费）。国际版只支持流式，故用 stream=true。
func intlProbeInfer(cm *CredentialManager, p *Proxy) probeResult {
	headers, err := cm.Headers()
	if err != nil {
		return probeUnknown
	}
	headers["User-Agent"] = p.UserAgent
	headers["Content-Type"] = "application/json"
	body, _ := json.Marshal(map[string]any{
		"model": "hy4-preview", // 免费模型，探活不花钱
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
		"stream":     true,
	})

	req, err := http.NewRequest("POST", p.Backend+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		return probeUnknown
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return probeUnknown
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// 读到第一帧即可判定通道可用（避免等完整生成）
		buf := make([]byte, 512)
		if _, err := resp.Body.Read(buf); err == nil {
			return probeOK
		}
		return probeOK // 200 且已建立流式连接
	}
	// 非 200：读错误体判类型
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var em map[string]any
	if json.Unmarshal(b, &em) == nil {
		if c, ok := em["code"].(float64); ok {
			switch int(c) {
			case 11140:
				return probeRisk
			case 6004:
				return probeRateLimited
			}
		}
		if msg, _ := em["msg"].(string); strings.Contains(msg, "frequency limit") {
			return probeRateLimited
		}
	}
	return probeUnknown
}

// intlCreditsRaw 查一个国际凭证的余额（复用 wbCredits 的接口与头）。
func intlCreditsRaw(cm *CredentialManager, p *Proxy) (remain, total float64, ok bool) {
	headers, err := cm.Headers()
	if err != nil {
		return 0, 0, false
	}
	headers["User-Agent"] = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	headers["X-Product"] = "SaaS"
	headers["X-IDE-Type"] = "WorkBuddy"
	body, status, err := httpPostJSON(p.Backend+"/billing/meter/get-user-resource-summary", headers, []byte("{}"))
	if err != nil || status != 200 {
		return 0, 0, false
	}
	return wbSummaryParse(body)
}

// sortStrings 简易字典序（避免为几行逻辑再引 sort 包到本文件）。
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}


// intlProbeInferWithURL 测试用：直接打指定 base URL 探活（生产走 intlProbeInfer）。
func intlProbeInferWithURL(base string) probeResult {
	body, _ := json.Marshal(map[string]any{
		"model": "hy4-preview",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"max_tokens": 1, "stream": true,
	})
	req, err := http.NewRequest("POST", base+"/v2/chat/completions", bytes.NewReader(body))
	if err != nil {
		return probeUnknown
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return probeUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		buf := make([]byte, 512)
		resp.Body.Read(buf)
		return probeOK
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var em map[string]any
	if json.Unmarshal(b, &em) == nil {
		if c, ok := em["code"].(float64); ok {
			switch int(c) {
			case 11140:
				return probeRisk
			case 6004:
				return probeRateLimited
			}
		}
		if msg, _ := em["msg"].(string); strings.Contains(msg, "frequency limit") {
			return probeRateLimited
		}
	}
	return probeUnknown
}
