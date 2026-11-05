package main

// 客户端心跳模拟（2026-11-05 用户要求「加客户端心跳模拟」）。
//
// 为什么需要：官方桌面端在没人聊天时也不是完全静默 —— 它会周期性拉配置目录、
// 刷一次额度、检查模型列表（5~10 分钟一次，带抖动）。而反代只在你发消息时才出网，
// 形状是「长时间零流量 + 密集对话突发」，这本身就是 Bot 特征。2026-10-25 的复盘里，
// 上游判据写的是「请求指纹 + system 身份句 + 请求节奏」的组合（见
// docs/2026-10-25-封号风险与出站身份对齐.md），TLS 指纹由 tlsfp.go 补，
// 这里补「节奏」。
//
// 设计原则（保号优先，宁可少发不可多发）：
//  1. 只发**只读**请求，而且是官方客户端本来就会发的那几条（目录 / 模型列表 / 积分），
//     不发聊天、不签到、不动 token；
//  2. 间隔带抖动（默认 4~9 分钟），不是整点定时 —— 定时器式的等间隔本身就是机器特征；
//  3. 夜间静默（默认 01:00~07:00 不发），真有请求也不补；
//  4. 空闲闸门：最近 90 秒内有真实请求就跳过（官方客户端在用户活跃时不会额外轮询）；
//  5. 每日上限（默认 120 次），超了只记状态不发；
//  6. 任何一条线路探针失败只记账，不重试、不放大（重试风暴正是 10-25 那次 11128 的放大器）。
//
// 开关：
//
//	AIHUB_HEARTBEAT=0            关闭
//	AIHUB_HEARTBEAT_MIN=240      最小间隔（秒）
//	AIHUB_HEARTBEAT_MAX=540      最大间隔（秒）
//	AIHUB_HEARTBEAT_IDLE=90      真实流量后的静默秒数
//	AIHUB_HEARTBEAT_MAXDAY=120   每日上限
//	AIHUB_HEARTBEAT_QUIET_FROM=1 静默开始（小时）
//	AIHUB_HEARTBEAT_QUIET_TO=7   静默结束（小时）
//
// 也可以写 <数据根>/heartbeat.json（控制台 POST /api/heartbeat 会落盘）：
//
//	{"enabled":true,"min_interval_sec":240,"max_interval_sec":540,
//	 "idle_gate_sec":90,"max_per_day":120,"quiet_from_hour":1,"quiet_to_hour":7}
//
// 环境变量优先级高于文件（便于临时压测：AIHUB_HEARTBEAT_MIN=5 AIHUB_HEARTBEAT_MAX=10）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 配置 ----------

type heartbeatSettings struct {
	Enabled        bool `json:"enabled"`
	MinIntervalSec int  `json:"min_interval_sec"`
	MaxIntervalSec int  `json:"max_interval_sec"`
	IdleGateSec    int  `json:"idle_gate_sec"`
	MaxPerDay      int  `json:"max_per_day"`
	QuietFromHour  int  `json:"quiet_from_hour"`
	QuietToHour    int  `json:"quiet_to_hour"`

	// envCadence=true 表示 MIN/MAX 被环境变量显式指定过：此时全局值优先，
	// 不再套用线路画像的节奏（运维手动钉的间隔应该说话算数）。
	envCadence bool `json:"-"`
}

func defaultHeartbeatSettings() heartbeatSettings {
	return heartbeatSettings{
		Enabled:        true,
		MinIntervalSec: 240, // 4 分钟
		MaxIntervalSec: 540, // 9 分钟
		IdleGateSec:    90,
		MaxPerDay:      120,
		QuietFromHour:  1,
		QuietToHour:    7,
	}
}

// heartbeatSettingsPath 心跳配置的落盘位置（跟其他运行数据同目录）。
func heartbeatSettingsPath() string { return dataPath("heartbeat.json") }

func loadHeartbeatSettings() heartbeatSettings {
	s := defaultHeartbeatSettings()
	if b, err := os.ReadFile(heartbeatSettingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	applyHeartbeatEnv(&s)
	return s.normalized()
}

// applyHeartbeatEnv 环境变量覆盖（临时调参不用改文件）。
func applyHeartbeatEnv(s *heartbeatSettings) {
	if v, ok := os.LookupEnv("AIHUB_HEARTBEAT"); ok && strings.TrimSpace(v) != "" {
		// 注意：空字符串按「没设」处理。启动脚本里 `AIHUB_HEARTBEAT=` 留空是常见写法，
		// 不该被当成「关闭」。
		s.Enabled = !isOffValue(strings.TrimSpace(v))
	}
	setInt := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	if _, ok := os.LookupEnv("AIHUB_HEARTBEAT_MIN"); ok {
		s.envCadence = true
	}
	if _, ok := os.LookupEnv("AIHUB_HEARTBEAT_MAX"); ok {
		s.envCadence = true
	}
	setInt("AIHUB_HEARTBEAT_MIN", &s.MinIntervalSec)
	setInt("AIHUB_HEARTBEAT_MAX", &s.MaxIntervalSec)
	setInt("AIHUB_HEARTBEAT_IDLE", &s.IdleGateSec)
	setInt("AIHUB_HEARTBEAT_MAXDAY", &s.MaxPerDay)
	setInt("AIHUB_HEARTBEAT_QUIET_FROM", &s.QuietFromHour)
	setInt("AIHUB_HEARTBEAT_QUIET_TO", &s.QuietToHour)
}

func isOffValue(v string) bool {
	switch v {
	case "", "0", "false", "False", "no", "off", "disable", "disabled":
		return true
	}
	return false
}

// normalized 把明显不合理的值拉回安全区间：
// 间隔 < 60s 的心跳不是「像人」，是「像压测」，直接抬到 60s 起。
func (s heartbeatSettings) normalized() heartbeatSettings {
	if s.MinIntervalSec < 60 {
		s.MinIntervalSec = 60
	}
	if s.MaxIntervalSec < s.MinIntervalSec {
		s.MaxIntervalSec = s.MinIntervalSec
	}
	if s.MaxIntervalSec > 3600 {
		s.MaxIntervalSec = 3600
	}
	if s.IdleGateSec < 0 {
		s.IdleGateSec = 0
	}
	if s.MaxPerDay < 0 {
		s.MaxPerDay = 0
	}
	if s.QuietFromHour < 0 || s.QuietFromHour > 23 {
		s.QuietFromHour = 0
	}
	if s.QuietToHour < 0 || s.QuietToHour > 23 {
		s.QuietToHour = 0
	}
	return s
}

// ---------- 状态 ----------

type heartbeatLineState struct {
	Line       string    `json:"line"`
	Label      string    `json:"label"`
	LastAt     time.Time `json:"last_at"`
	LastOK     bool      `json:"last_ok"`
	LastDetail string    `json:"last_detail,omitempty"`
	Count      int       `json:"count"`
	OK         int       `json:"ok"`
	Fail       int       `json:"fail"`
	ConsecFail int       `json:"consec_fail"`
}

type heartbeatState struct {
	mu        sync.Mutex
	cfg       heartbeatSettings
	startedAt time.Time
	lines     map[string]*heartbeatLineState
	day       string
	dayCount  int
	lastTick  time.Time
	lastSkip  string
	lastRun   time.Time
	totalRuns int
	// lastProbeLine 最近一次真正发出去的探针线路（用来给下一次挑节奏）。
	lastProbeLine string
	stopped       bool
	stopCh        chan struct{}
}

// activityState 记录「最近一次真实请求」，用于空闲闸门。
// 由 usage_stats.go:recordUsage 与各线路监听层（lineActivityMiddleware）写入。
var activityState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func noteRealTraffic(line string) {
	if line == "" {
		return
	}
	activityState.mu.Lock()
	if activityState.last == nil {
		activityState.last = map[string]time.Time{}
	}
	activityState.last[line] = time.Now()
	activityState.mu.Unlock()
}

// realTrafficWithin 该线路（或任意线路，line 为空）最近 d 内是否有真实请求。
func realTrafficWithin(line string, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	cutoff := time.Now().Add(-d)
	activityState.mu.Lock()
	defer activityState.mu.Unlock()
	for k, at := range activityState.last {
		if line != "" && k != line {
			continue
		}
		if at.After(cutoff) {
			return true
		}
	}
	return false
}

func newHeartbeatState() *heartbeatState {
	return &heartbeatState{
		cfg:       loadHeartbeatSettings(),
		startedAt: time.Now(),
		lines:     map[string]*heartbeatLineState{},
		day:       time.Now().Format("2006-01-02"),
		stopCh:    make(chan struct{}),
	}
}

// ---------- 探针 ----------

// heartbeatProbe 一条只读探针。Run 返回给日志/看板看的简短结果。
type heartbeatProbe struct {
	Line  string
	Label string
	Run   func(ctx context.Context) (string, error)
}

// heartbeatProbes 当前**正在运行**的线路各自能发的只读请求。
//
// 只覆盖有「官方客户端本来就会打的只读接口」的线路：
//
//	dom / intl  → GET /v3/config（目录，桌面端启动与定时刷新都打）
//	qoder       → GET algo/api/v2/model/list（模型目录）
//	zcode       → GET api/v1/client/configs（客户端配置）
//	teleagent   → GET /superCowork/sapi/user/portal/points/detail（积分）
//
// 不覆盖：
//
//	trae  —— 唯一周期动作是 ExchangeToken，会轮换 refreshToken，和真实对话请求抢，
//	         插进去可能把用户的 token 弄失效，宁可不发；
//	oc    —— 匿名 public 凭据，没有「账号」可保护；
//	mimo  —— 线路本身没打通。
func (h *Hub) heartbeatProbes() []heartbeatProbe {
	h.mu.Lock()
	dom, intl, qoder, zcode, teleagent := h.dom, h.intl, h.qoder, h.zcode, h.teleagent
	h.mu.Unlock()

	out := make([]heartbeatProbe, 0, 5)

	if dom != nil && dom.Running() {
		p := dom
		out = append(out, heartbeatProbe{Line: "dom", Label: "上游目录 /v3/config", Run: func(ctx context.Context) (string, error) {
			n, err := heartbeatWBCatalogPull("dom", p)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d 个条目", n), nil
		}})
	}
	if intl != nil && intl.Running() {
		p := intl
		out = append(out, heartbeatProbe{Line: "intl", Label: "上游目录 /v3/config", Run: func(ctx context.Context) (string, error) {
			n, err := heartbeatWBCatalogPull("intl", p)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d 个条目", n), nil
		}})
	}
	if qoder != nil && qoder.Running() {
		p := qoder
		out = append(out, heartbeatProbe{Line: "qoder", Label: "模型目录 /algo/api/v2/model/list", Run: func(ctx context.Context) (string, error) {
			p.mu.Lock()
			auth, cli := p.auth, p.client
			p.mu.Unlock()
			if auth == nil || cli == nil {
				return "", errors.New("凭据未就绪")
			}
			entries, err := fetchQoderCatalog(ctx, auth, cli, QoderModelListURL)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d 个模型", len(entries)), nil
		}})
	}
	if zcode != nil && zcode.Running() {
		p := zcode
		out = append(out, heartbeatProbe{Line: "zcode", Label: "客户端配置 /api/v1/client/configs", Run: func(ctx context.Context) (string, error) {
			entries, err := p.fetchRemoteCatalog()
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d 个模型", len(entries)), nil
		}})
	}
	if teleagent != nil && teleagent.Running() {
		out = append(out, heartbeatProbe{Line: "teleagent", Label: "积分 /user/portal/points/detail", Run: func(ctx context.Context) (string, error) {
			token, err := teleagentToken()
			if err != nil {
				return "", err
			}
			dev, ins, err := teleagentDeviceMeta()
			if err != nil {
				return "", err
			}
			var points teleagentPointsPayload
			if info := teleagentGetJSON(token, dev, ins, "/user/portal/points/detail", &points); info != nil {
				return "", errors.New(info.Text)
			}
			return fmt.Sprintf("积分 %.2f", points.Data.PointsBalance), nil
		}})
	}
	return out
}

// heartbeatWBCatalogPull 只发一次 GET /v3/config 并解析，**不落盘、不重写 Codex 目录**。
// 与 refreshWBCatalog 的区别只在副作用：心跳要的是「上游看见一次官方客户端在拉目录」，
// 不需要顺带刷新 wb_catalog_*.json / ~/.codex 的目录文件（那些由看板与启动预热负责，
// 心跳每 4~9 分钟重写一次纯属多余的磁盘写）。
func heartbeatWBCatalogPull(line string, p *Proxy) (int, error) {
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		return 0, errors.New("凭据文件不存在")
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	headers, err := cm.Headers()
	if err != nil {
		return 0, fmt.Errorf("取认证头失败：%w", err)
	}
	client := newBackendClientFor(line)
	client.Timeout = 20 * time.Second
	cat, err := fetchWBCatalog(client, p.Backend, line, headers)
	if err != nil {
		return 0, err
	}
	return len(cat.Entries), nil
}

// heartbeatUnsupportedLines 说明哪些线路没有探针、为什么（给看板看，避免「以为漏了」）。
func heartbeatUnsupportedLines() map[string]string {
	return map[string]string{
		"trae": "唯一周期动作是 ExchangeToken（会轮换 refreshToken，和真实请求抢），不发心跳",
		"oc":   "匿名 public 凭据，没有账号可保护",
		"mimo": "线路未打通",
	}
}

// ---------- 调度 ----------

// StartHeartbeat 启动心跳协程。可重复调用，只会起一个。
func (h *Hub) StartHeartbeat() {
	h.mu.Lock()
	if h.hb != nil {
		h.mu.Unlock()
		return
	}
	hb := newHeartbeatState()
	h.hb = hb
	h.mu.Unlock()

	if !hb.cfg.Enabled {
		logHub("[heartbeat] 已关闭（AIHUB_HEARTBEAT=0 或 heartbeat.json enabled=false）")
		return
	}
	logHub("[heartbeat] 已启用：间隔 %d~%ds，静默 %02d:00~%02d:00，每日上限 %d 次，空闲闸门 %ds",
		hb.cfg.MinIntervalSec, hb.cfg.MaxIntervalSec, hb.cfg.QuietFromHour, hb.cfg.QuietToHour, hb.cfg.MaxPerDay, hb.cfg.IdleGateSec)
	go h.heartbeatLoop(hb)
}

// heartbeatStopCh 暴露心跳的停止信号，供其它后台轮询复用（进程退出时统一收尾）。
// 心跳没启动时返回一个永不关闭的通道。
func (h *Hub) heartbeatStopCh() <-chan struct{} {
	h.mu.Lock()
	hb := h.hb
	h.mu.Unlock()
	if hb == nil {
		return make(chan struct{})
	}
	return hb.stopCh
}

// StopHeartbeat 停止心跳（进程退出/停止全部线路时用；幂等）。
func (h *Hub) StopHeartbeat() {
	h.mu.Lock()
	hb := h.hb
	h.mu.Unlock()
	if hb == nil {
		return
	}
	hb.mu.Lock()
	if !hb.stopped {
		hb.stopped = true
		close(hb.stopCh)
	}
	hb.mu.Unlock()
}

func (h *Hub) heartbeatLoop(hb *heartbeatState) {
	defer recoverGoroutine("heartbeat.go:loop")
	for {
		hb.mu.Lock()
		stopped := hb.stopped
		cfg := hb.cfg
		hb.mu.Unlock()
		if stopped {
			return
		}
		hb.mu.Lock()
		lastLine := hb.lastProbeLine
		hb.mu.Unlock()
		delay := heartbeatDelayForLine(cfg, lastLine)
		t := time.NewTimer(delay)
		hb.mu.Lock()
		hb.lastTick = time.Now().Add(delay)
		hb.mu.Unlock()
		select {
		case <-t.C:
		case <-hb.stopCh:
			t.Stop()
			return
		}
		h.heartbeatTick(hb)
	}
}

// heartbeatCadenceFor 取线路画像里的心跳节奏。ok=false 表示这条线路没有画像，
// 或画像没配节奏（用全局值）。
func heartbeatCadenceFor(line string) (minSec, maxSec int, ok bool) {
	w, found := clientWires[line]
	if !found || !w.Heartbeat || w.HBMin <= 0 || w.HBMax <= 0 {
		return 0, 0, false
	}
	return w.HBMin, w.HBMax, true
}

// heartbeatDelayForLine 取下一次心跳的间隔。
// 优先级：环境变量显式指定（envCadence） > 线路画像 > 全局默认。
func heartbeatDelayForLine(cfg heartbeatSettings, line string) time.Duration {
	if !cfg.envCadence {
		if minSec, maxSec, ok := heartbeatCadenceFor(line); ok {
			return jitterSeconds(minSec, maxSec)
		}
	}
	return heartbeatDelay(cfg)
}

func jitterSeconds(min, max int) time.Duration {
	if max <= min {
		return time.Duration(min) * time.Second
	}
	return time.Duration(min+rand.Intn(max-min+1)) * time.Second
}

// heartbeatDelay 在 [min,max] 里取一个抖动间隔。
func heartbeatDelay(cfg heartbeatSettings) time.Duration {
	min := cfg.MinIntervalSec
	max := cfg.MaxIntervalSec
	if max <= min {
		return time.Duration(min) * time.Second
	}
	n := min + rand.Intn(max-min+1)
	return time.Duration(n) * time.Second
}

// heartbeatTick 一次调度：挑一条线路发一条只读请求。
func (h *Hub) heartbeatTick(hb *heartbeatState) {
	hb.runHeartbeatTick(h.heartbeatProbes())
}

// runHeartbeatTick 调度主体（探针由调用方注入，便于测试）。
// 任何一步不满足（关闭/静默/超量/刚有真实请求/没线路在跑）都只记 skip 原因。
func (hb *heartbeatState) runHeartbeatTick(probes []heartbeatProbe) {
	hb.mu.Lock()
	cfg := hb.cfg
	hb.mu.Unlock()
	if !cfg.Enabled {
		hb.setSkip("已关闭")
		return
	}
	now := time.Now()
	if heartbeatQuiet(now, cfg) {
		hb.setSkip(fmt.Sprintf("静默时段 %02d:00~%02d:00", cfg.QuietFromHour, cfg.QuietToHour))
		return
	}
	hb.bumpDayIfNeeded(now)
	hb.mu.Lock()
	dayCount := hb.dayCount
	hb.mu.Unlock()
	if cfg.MaxPerDay > 0 && dayCount >= cfg.MaxPerDay {
		hb.setSkip(fmt.Sprintf("今日已发 %d 次，达上限 %d", dayCount, cfg.MaxPerDay))
		return
	}
	if len(probes) == 0 {
		hb.setSkip("没有在跑的线路（或无可用只读探针）")
		return
	}
	probe := hb.pickProbe(probes)
	hb.mu.Lock()
	hb.lastProbeLine = probe.Line
	hb.mu.Unlock()
	if realTrafficWithin(probe.Line, time.Duration(cfg.IdleGateSec)*time.Second) {
		hb.setSkip(probe.Line + " 最近有真实请求，本次跳过")
		hb.markSkip(probe, "跳过：真实请求刚发生过")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t0 := time.Now()
	detail, err := probe.Run(ctx)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		hb.finishProbe(probe, false, truncate(err.Error(), 160))
		logHub("[heartbeat] %s 失败（%dms）：%v", probe.Line, ms, err)
		return
	}
	hb.finishProbe(probe, true, detail)
	logHub("[heartbeat] %s %s → %s（%dms）", probe.Line, probe.Label, detail, ms)
}

func heartbeatQuiet(t time.Time, cfg heartbeatSettings) bool {
	hour := t.Hour()
	if cfg.QuietFromHour == cfg.QuietToHour {
		return false
	}
	if cfg.QuietFromHour < cfg.QuietToHour {
		return hour >= cfg.QuietFromHour && hour < cfg.QuietToHour
	}
	// 跨午夜，如 23 → 6
	return hour >= cfg.QuietFromHour || hour < cfg.QuietToHour
}

func (hb *heartbeatState) bumpDayIfNeeded(now time.Time) bool {
	day := now.Format("2006-01-02")
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.day == day {
		return false
	}
	hb.day = day
	hb.dayCount = 0
	return true
}

// pickProbe 选「最久没发过」的线路；都没发过就按顺序取第一条。
func (hb *heartbeatState) pickProbe(probes []heartbeatProbe) heartbeatProbe {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	best := probes[0]
	var bestAt time.Time
	for _, p := range probes {
		st := hb.lines[p.Line]
		if st == nil || st.LastAt.IsZero() {
			return p
		}
		if bestAt.IsZero() || st.LastAt.Before(bestAt) {
			best, bestAt = p, st.LastAt
		}
	}
	return best
}

func (hb *heartbeatState) setSkip(reason string) {
	hb.mu.Lock()
	hb.lastSkip = reason
	hb.mu.Unlock()
	if tlsFPDebug() {
		logHub("[heartbeat] skip：%s", reason)
	}
}

// markSkip 只更新状态，不发请求（用于「跳过」记账）。
func (hb *heartbeatState) markSkip(p heartbeatProbe, detail string) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	st := hb.lineStateLocked(p)
	st.Label = p.Label
	st.LastDetail = detail
}

func (hb *heartbeatState) finishProbe(p heartbeatProbe, ok bool, detail string) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	st := hb.lineStateLocked(p)
	st.Label = p.Label
	st.LastAt = time.Now()
	st.LastOK = ok
	st.LastDetail = detail
	st.Count++
	hb.totalRuns++
	hb.lastRun = time.Now()
	hb.lastSkip = ""
	if ok {
		st.OK++
		st.ConsecFail = 0
		hb.dayCount++
	} else {
		st.Fail++
		st.ConsecFail++
	}
}

func (hb *heartbeatState) lineStateLocked(p heartbeatProbe) *heartbeatLineState {
	st := hb.lines[p.Line]
	if st == nil {
		st = &heartbeatLineState{Line: p.Line}
		hb.lines[p.Line] = st
	}
	return st
}

// ---------- 视图 / API ----------

func (h *Hub) heartbeatStatus() map[string]any {
	h.mu.Lock()
	hb := h.hb
	h.mu.Unlock()
	if hb == nil {
		return map[string]any{"enabled": false, "state": "未启动"}
	}
	hb.mu.Lock()
	defer hb.mu.Unlock()
	lines := make([]heartbeatLineState, 0, len(hb.lines))
	for _, st := range hb.lines {
		lines = append(lines, *st)
	}
	return map[string]any{
		"enabled":     hb.cfg.Enabled,
		"settings":    hb.cfg,
		"started_at":  hb.startedAt,
		"day":         hb.day,
		"day_count":   hb.dayCount,
		"total_runs":  hb.totalRuns,
		"last_run":    hb.lastRun,
		"next_tick":   hb.lastTick,
		"last_skip":   hb.lastSkip,
		"lines":       lines,
		"unsupported": heartbeatUnsupportedLines(),
	}
}

// apiHeartbeat GET=读状态；POST=改配置（落盘 heartbeat.json，下次 tick 生效）。
func (h *Hub) apiHeartbeat(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "heartbeat": h.heartbeatStatus()})
	case http.MethodPost:
		patch := struct {
			Enabled        *bool `json:"enabled"`
			MinIntervalSec *int  `json:"min_interval_sec"`
			MaxIntervalSec *int  `json:"max_interval_sec"`
			IdleGateSec    *int  `json:"idle_gate_sec"`
			MaxPerDay      *int  `json:"max_per_day"`
			QuietFromHour  *int  `json:"quiet_from_hour"`
			QuietToHour    *int  `json:"quiet_to_hour"`
		}{}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "请求体不是合法 JSON：" + err.Error()})
			return
		}
		h.mu.Lock()
		hb := h.hb
		h.mu.Unlock()
		if hb == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "心跳未启动"})
			return
		}
		hb.mu.Lock()
		cfg := hb.cfg
		if patch.Enabled != nil {
			cfg.Enabled = *patch.Enabled
		}
		if patch.MinIntervalSec != nil {
			cfg.MinIntervalSec = *patch.MinIntervalSec
		}
		if patch.MaxIntervalSec != nil {
			cfg.MaxIntervalSec = *patch.MaxIntervalSec
		}
		if patch.IdleGateSec != nil {
			cfg.IdleGateSec = *patch.IdleGateSec
		}
		if patch.MaxPerDay != nil {
			cfg.MaxPerDay = *patch.MaxPerDay
		}
		if patch.QuietFromHour != nil {
			cfg.QuietFromHour = *patch.QuietFromHour
		}
		if patch.QuietToHour != nil {
			cfg.QuietToHour = *patch.QuietToHour
		}
		cfg = cfg.normalized()
		hb.cfg = cfg
		hb.mu.Unlock()

		if err := saveHeartbeatSettings(cfg); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "已生效但落盘失败（重启后会丢）：" + err.Error(), "settings": cfg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "心跳配置已更新", "settings": cfg})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "msg": "只支持 GET/POST"})
	}
}

func saveHeartbeatSettings(cfg heartbeatSettings) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := heartbeatSettingsPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, heartbeatSettingsPath())
}

// apiTLSFingerprint 只读展示出站 TLS 指纹配置与最近握手结果。
func (h *Hub) apiTLSFingerprint(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tls_fingerprint": tlsFingerprintSummary()})
}
