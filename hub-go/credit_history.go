package main

// 每日积分快照：回答「昨天上报的活跃，30 分到账了吗」。
//
// 背景（2026-10-12 用户要求）：国际版的每日活跃奖励「通常次日发放」，所以
// 当天点完签到看不出效果，要等第二天。与其让用户去官网翻积分明细，不如
// hub-go 自己记一份每日余额，直接显示「较昨日 +30」。
//
// 设计要点：
//   - 按 **uid**（账号身份）分桶，不按凭证目录名 —— 同一账号可能放在不同目录，
//     目录名也会变（用户可自起名），uid 才是不变的主键。
//   - 每天一条，同日重复记录**覆盖**（取当天最后一次的值，即当日最终余额）。
//   - 只保留最近 N 天，避免文件无限增长。
//   - 写盘用 tmp+rename 原子替换，避免与并发读取（控制台轮询）撞车。

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

const creditHistoryFile = "credit_history.json"

// creditHistoryDays 保留天数。只用于「对比昨天」，30 天足够回看。
const creditHistoryDays = 30

// creditSnapshot 某账号某天的积分快照。
type creditSnapshot struct {
	Remain float64 `json:"remain"`
	Total  float64 `json:"total"`
}

// creditHistoryEntry 一个账号的历史：日期 → 快照。
type creditHistoryEntry struct {
	Name string                    `json:"name,omitempty"` // 最近一次见到的昵称，便于辨认
	Days map[string]creditSnapshot `json:"days"`
}

var creditHistoryState = struct {
	mu      sync.Mutex
	loaded  bool
	entries map[string]*creditHistoryEntry // uid → 历史
}{entries: map[string]*creditHistoryEntry{}}

func creditHistoryPath() string { return dataPath(creditHistoryFile) }

// loadCreditHistoryLocked 读盘（只调用一次）。坏文件当空处理，不影响主流程。
func loadCreditHistoryLocked() {
	if creditHistoryState.loaded {
		return
	}
	creditHistoryState.loaded = true
	b, err := os.ReadFile(creditHistoryPath())
	if err != nil {
		return
	}
	var raw struct {
		Accounts map[string]*creditHistoryEntry `json:"accounts"`
	}
	if json.Unmarshal(b, &raw) != nil || raw.Accounts == nil {
		return
	}
	for uid, e := range raw.Accounts {
		if e == nil {
			continue
		}
		if e.Days == nil {
			e.Days = map[string]creditSnapshot{}
		}
		creditHistoryState.entries[uid] = e
	}
}

// saveCreditHistoryLocked 落盘（调用方需持锁）。原子替换。
func saveCreditHistoryLocked() {
	raw := struct {
		Accounts map[string]*creditHistoryEntry `json:"accounts"`
	}{Accounts: creditHistoryState.entries}
	b, err := json.MarshalIndent(raw, "", " ")
	if err != nil {
		return
	}
	path := creditHistoryPath()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// creditHistoryRecord 记录今天这个账号的余额快照（同日覆盖）。
// uid 为空时直接跳过 —— 没有身份就无法归属，宁可不记。
func creditHistoryRecord(uid, name string, remain, total float64) {
	if uid == "" {
		return
	}
	today := time.Now().Format("2006-01-02")

	creditHistoryState.mu.Lock()
	defer creditHistoryState.mu.Unlock()
	loadCreditHistoryLocked()

	e := creditHistoryState.entries[uid]
	if e == nil {
		e = &creditHistoryEntry{Days: map[string]creditSnapshot{}}
		creditHistoryState.entries[uid] = e
	}
	if name != "" {
		e.Name = name
	}
	// 同日重复：只在余额变化时写盘，避免每次轮询都落盘。
	if old, ok := e.Days[today]; ok && old.Remain == remain && old.Total == total {
		return
	}
	e.Days[today] = creditSnapshot{Remain: remain, Total: total}

	// 裁剪：只留最近 creditHistoryDays 天
	if len(e.Days) > creditHistoryDays {
		cutoff := time.Now().AddDate(0, 0, -creditHistoryDays).Format("2006-01-02")
		for d := range e.Days {
			if d < cutoff {
				delete(e.Days, d)
			}
		}
	}
	saveCreditHistoryLocked()
}

// creditHistoryPrev 取「今天之前最近一天」的快照，用于算差额。
// 返回 (快照, 那天日期, 是否找到)。今天尚无记录时也返回上一日，便于显示基线。
func creditHistoryPrev(uid string) (creditSnapshot, string, bool) {
	if uid == "" {
		return creditSnapshot{}, "", false
	}
	today := time.Now().Format("2006-01-02")

	creditHistoryState.mu.Lock()
	defer creditHistoryState.mu.Unlock()
	loadCreditHistoryLocked()

	e := creditHistoryState.entries[uid]
	if e == nil {
		return creditSnapshot{}, "", false
	}
	best, bestDay := "", ""
	for d := range e.Days {
		if d < today && d > bestDay {
			best, bestDay = d, d
		}
	}
	if best == "" {
		return creditSnapshot{}, "", false
	}
	return e.Days[best], bestDay, true
}

// creditHistoryToday 取今天的快照（若已记录）。
func creditHistoryToday(uid string) (creditSnapshot, bool) {
	if uid == "" {
		return creditSnapshot{}, false
	}
	today := time.Now().Format("2006-01-02")
	creditHistoryState.mu.Lock()
	defer creditHistoryState.mu.Unlock()
	loadCreditHistoryLocked()
	e := creditHistoryState.entries[uid]
	if e == nil {
		return creditSnapshot{}, false
	}
	s, ok := e.Days[today]
	return s, ok
}

// creditHistoryDiffText 生成「较 <日期> +30 / -5」这类文案；无从比较时返回空串。
// 这是回答「活跃奖励到账了吗」的核心：昨天上报、今天到账，差额应 ≈ +30。
func creditHistoryDiffText(uid string) string {
	today, ok := creditHistoryToday(uid)
	if !ok {
		return ""
	}
	prev, day, ok := creditHistoryPrev(uid)
	if !ok {
		return ""
	}
	d := today.Remain - prev.Remain
	if d == 0 {
		return "较 " + day[5:] + " 持平"
	}
	// 注意：不能直接 fmtCN(d)——它按 int64(f+0.5) 取整，对负数会把
	// -30 变成 -29（-30+0.5 = -29.5 → -29）。这里先取绝对值，符号单独加。
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return "较 " + day[5:] + " " + sign + fmtCN(d)
}

// creditDayView 一天的余额与「消耗 / 到账」拆解。
//
// 只靠余额差值分不清「花掉了」和「充值了」，所以用总额度一起看：
//
//	Granted  = max(0, total - prevTotal)      ← 总额涨了 = 充值/赠送
//	Consumed = Granted - (remain - prevRemain) ← 余额减少超过到账的部分就是花掉的
//
// 首次出现的日期没有基线，两项都记 0（只做展示起点）。
type creditDayView struct {
	Date     string  `json:"date"`
	Remain   float64 `json:"remain"`
	Total    float64 `json:"total"`
	Consumed float64 `json:"consumed"`
	Granted  float64 `json:"granted"`
}

// creditAccountSeries 一个账号的序列。
type creditAccountSeries struct {
	UID           string          `json:"uid"`
	Name          string          `json:"name"`
	Latest        creditSnapshot  `json:"latest"`
	TodayUsed     float64         `json:"today_used"`
	PeriodUsed    float64         `json:"period_used"`
	PeriodGranted float64         `json:"period_granted"`
	Days          []creditDayView `json:"days"`
}

// creditHistorySeries 取全部账号最近 days 天的余额序列（按日期升序）。
func creditHistorySeries(days int) []creditAccountSeries {
	if days <= 0 {
		days = 7
	}
	if days > creditHistoryDays {
		days = creditHistoryDays
	}
	creditHistoryState.mu.Lock()
	loadCreditHistoryLocked()
	uids := make([]string, 0, len(creditHistoryState.entries))
	for uid := range creditHistoryState.entries {
		uids = append(uids, uid)
	}
	// 快照一份，避免持有锁期间做排序/计算
	type acct struct {
		name string
		days map[string]creditSnapshot
	}
	tmp := make(map[string]acct, len(uids))
	for _, uid := range uids {
		e := creditHistoryState.entries[uid]
		if e == nil {
			continue
		}
		cp := make(map[string]creditSnapshot, len(e.Days))
		for d, v := range e.Days {
			cp[d] = v
		}
		tmp[uid] = acct{name: e.Name, days: cp}
	}
	creditHistoryState.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	today := time.Now().Format("2006-01-02")
	out := make([]creditAccountSeries, 0, len(tmp))
	for uid, a := range tmp {
		dates := make([]string, 0, len(a.days))
		for d := range a.days {
			if d >= cutoff {
				dates = append(dates, d)
			}
		}
		if len(dates) == 0 {
			continue
		}
		// 升序（日期是 YYYY-MM-DD，字符串序即时间序）
		for i := 1; i < len(dates); i++ {
			for j := i; j > 0 && dates[j] < dates[j-1]; j-- {
				dates[j], dates[j-1] = dates[j-1], dates[j]
			}
		}
		item := creditAccountSeries{UID: uid, Name: a.name}
		var prev *creditSnapshot
		for _, d := range dates {
			cur := a.days[d]
			view := creditDayView{Date: d, Remain: cur.Remain, Total: cur.Total}
			if prev != nil {
				granted := cur.Total - prev.Total
				if granted < 0 {
					granted = 0
				}
				used := granted - (cur.Remain - prev.Remain)
				if used < 0 {
					used = 0
				}
				view.Granted = granted
				view.Consumed = used
				item.PeriodGranted += granted
				item.PeriodUsed += used
				if d == today {
					item.TodayUsed = used
				}
			}
			snapshot := cur
			prev = &snapshot
			item.Days = append(item.Days, view)
		}
		item.Latest = a.days[dates[len(dates)-1]]
		out = append(out, item)
	}
	// 消耗多的排前面
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].PeriodUsed > out[j-1].PeriodUsed; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
