package main

// 国际线路消耗预警（2026-10-13，用户指定只做国际版）。
//
// 背景：桌面端标 0.00x 的「每日免费」模型实测在扣积分（<凭证目录B> 当天 190
// 全光，deepseek-v4.1-flash 按 0.11x 口径计费）——免费活动的覆盖范围/每日
// 额度不透明。用户要求：一旦发现"开始扣我积分"就提醒，好停手。
//
// 水位法记账：每次国际余额查询成功后，把当日见过的最低余额记下来；
// 余额比最低水位还低 = 免费失效、正在扣费，累计降幅就是当日扣费额。
// 签到/刷新包带来的上涨不回退预警（burn 只增不减，跨天重置）。
// 每个国际凭证（uid）独立记账。

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type intlBurnAccount struct {
	Date      string  `json:"date"`
	MinRemain float64 `json:"min_remain"`
	Burn      float64 `json:"burn"` // 当日已确认的扣费额（积分）
}

var (
	intlBurnMu sync.Mutex
	intlBurn   map[string]*intlBurnAccount // uid → 当日账目
)

func intlBurnPath() string { return dataPath("intl_burn.json") }

func loadIntlBurnLocked() {
	if intlBurn != nil {
		return
	}
	intlBurn = map[string]*intlBurnAccount{}
	b, err := os.ReadFile(intlBurnPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &intlBurn)
}

func saveIntlBurnLocked() {
	b, err := json.MarshalIndent(intlBurn, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(intlBurnPath(), b, 0644)
}

// recordIntlCreditWater 余额查询成功后更新水位，返回该凭证当日累计扣费。
func recordIntlCreditWater(uid string, remain float64) float64 {
	if uid == "" {
		return 0
	}
	intlBurnMu.Lock()
	defer intlBurnMu.Unlock()
	loadIntlBurnLocked()
	today := time.Now().Format("2006-01-02")
	a := intlBurn[uid]
	if a == nil || a.Date != today {
		a = &intlBurnAccount{Date: today, MinRemain: remain}
		intlBurn[uid] = a
	}
	if remain < a.MinRemain {
		a.Burn += a.MinRemain - remain
		a.MinRemain = remain
	}
	saveIntlBurnLocked()
	return a.Burn
}
