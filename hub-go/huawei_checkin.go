package main

// 果办（OfficeAce）签到 + 额度：接进 checkin.go 的 runCheckin 与 credits.go
// 的 fetchCredit，与 dom/qoder/trae/teleagent 同一口径。
//
// 端点（全部实测过，2026-11-12/13）：
//   签到  POST https://officeace.cn-southwest-2.huaweicloud-agentarts.com
//             /v1/subscription/bonus/claim        （V11 签名，无 body）
//   额度  GET  同域 /v1/subscription              （V11 签名）
//
// 签到语义（officeace-auth/magpie 同款六 outcome）：
//   2xx + bonus_skus 非空 = 领到（每项 points 求和）
//   2xx + bonus_skus 空   = 今日已领（幂等，不算失败）
//   403 OfficeAce.11020604 = 未订购套餐
//   其他非 2xx = 失败（透传错误码）
//
// 额度响应结构（SUBSCRIBED 时）：
//   { domain_id, subscribe_status, skus: [ { sku_code, sku_name,
//     effective_time, expired_time, quotas: [ { officeace_points:
//     "500", current_value: "0" }, { online_search_count: "200",
//     current_value: "0" } ] } ] }
//   注意 current_value 是**已用**量（officeace-auth fetchUsage 的语义：
//   quota max - current_value = 剩余；签到/新用户的 bonus 积分在独立的
//   bonus_skus 里，未并入 quota 的 current_value——所以面板显示
//   「套餐桶 + bonus 桶」两个数，别硬合成一个）。
//
// 幂等与风控口径与既有平台一致：只在用户点「一键签到」时触发，不自动定时
// （main.go:160 的既有决定，果办照办）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const hwSubscriptionBase = "https://officeace.cn-southwest-2.huaweicloud-agentarts.com"

// ---------- 签到 ----------

// hwCheckin 果办每日签到。vault 没账号时返回 OK=true + Detail 提示未登录
// （与 runCheckin 里 domCheckin 的「没凭证不拖垮整体」口径一致——签到芯片
// 显示灰提示，不让其他平台跟着变红）。
func hwCheckin() checkinResult {
	cred, _, err := hwFreshSTS()
	if err != nil {
		if strings.Contains(err.Error(), "未找到果办账号") {
			return checkinResult{OK: true, Detail: "果办未登录（跳过）"}
		}
		return checkinResult{OK: false, Detail: "果办凭据：" + err.Error()}
	}
	body, status, err := httpPostJSON(hwSubscriptionBase+"/v1/subscription/bonus/claim",
		signHuaweiV11(cred, "POST", hwSubscriptionBase+"/v1/subscription/bonus/claim", []byte{}, nil), []byte{})
	if err != nil {
		return checkinResult{OK: false, Detail: "果办网络失败：" + err.Error()}
	}
	if status == http.StatusForbidden {
		return checkinResult{OK: false, Detail: "果办未订购套餐（403 OfficeAce.11020604）"}
	}
	if status != http.StatusOK {
		return checkinResult{OK: false, Detail: fmt.Sprintf("果办 HTTP %d：%s", status, truncate(string(body), 120))}
	}
	var j struct {
		BonusSkus []struct {
			ActivityID   string `json:"activity_id"`
			ActivityName string `json:"activity_name"`
			Points       string `json:"points"`
			ExpiredTime  string `json:"expired_time"`
		} `json:"bonus_skus"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		// 2xx 但解析失败：领取本身可能已成功（幂等），报 done 不报 failed
		return checkinResult{OK: true, Detail: "果办已领取（响应无法解析）"}
	}
	if len(j.BonusSkus) == 0 {
		return checkinResult{OK: true, Detail: "果办今日已领"}
	}
	total := 0
	for _, s := range j.BonusSkus {
		var p int
		fmt.Sscanf(s.Points, "%d", &p)
		total += p
	}
	names := make([]string, 0, len(j.BonusSkus))
	for _, s := range j.BonusSkus {
		if s.ActivityName != "" {
			names = append(names, s.ActivityName)
		}
	}
	return checkinResult{OK: true, Detail: fmt.Sprintf("果办签到成功 +%d 积分（%s）", total, strings.Join(names, "、"))}
}

// ---------- 额度 ----------

// hwCredits 果办额度查询。接进 fetchCredit 的 switch。
func hwCredits() *creditInfo {
	cred, _, err := hwFreshSTS()
	if err != nil {
		return &creditInfo{OK: false, Text: "果办凭据：" + err.Error()}
	}
	body, status, err := httpGetJSON(hwSubscriptionBase+"/v1/subscription",
		signHuaweiV11(cred, "GET", hwSubscriptionBase+"/v1/subscription", nil, nil))
	if err != nil {
		// 带上真实原因：只写「网络错误」会把「代理挂了」和「DNS 失败」
		// 混成同一条文案，排查时无从下手（本机代理随用户决定变动）。
		return &creditInfo{OK: false, Text: "查询失败（" + truncate(err.Error(), 120) + "）"}
	}
	switch {
	case status == http.StatusForbidden:
		return &creditInfo{OK: false, Text: "果办未订购套餐"}
	case status != http.StatusOK:
		return &creditInfo{OK: false, Text: fmt.Sprintf("查询失败（HTTP %d）", status)}
	}
	// 真实响应结构（2026-11-13 实测 dump，与 officeace-auth 的假设不同）：
	//   skus[].quotas[] 用 sku_attr_code + sku_value（不是 officeace_points 键）
	//   bonus 积分在**根层** bonus_skus[]（points 是额度，current_value 是已用小数）
	//   current_value 是已消耗量（浮点字符串，如 "0.00271542857142857"）
	var j struct {
		SubscribeStatus string `json:"subscribe_status"`
		DomainID        string `json:"domain_id"`
		Skus            []struct {
			SkuCode string `json:"sku_code"`
			SkuName string `json:"sku_name"`
			Expired string `json:"expired_time"`
			Quotas  []struct {
				SkuAttrCode string `json:"sku_attr_code"`
				SkuValue    string `json:"sku_value"`
				Current     string `json:"current_value"`
				Expired     string `json:"expired_time"`
			} `json:"quotas"`
		} `json:"skus"`
		BonusSkus []struct {
			ActivityName string `json:"activity_name"`
			Points       string `json:"points"`
			Current      string `json:"current_value"`
			Expired      string `json:"expired_time"`
		} `json:"bonus_skus"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return &creditInfo{OK: false, Text: "响应解析失败"}
	}
	if j.SubscribeStatus != "SUBSCRIBED" || len(j.Skus) == 0 {
		return &creditInfo{OK: true, Text: "果办未订购（" + j.SubscribeStatus + "）", FetchedAt: nowStamp()}
	}

	// 文案分两段：套餐桶（skus）与赠币桶（bonus_skus）。
	// 两个桶**不相加**——套餐是月额度、bonus 是活动赠币，各自有独立到期时间，
	// 合成一个数会让用户以为「今天花的是套餐额度」。
	//
	// 2026-11-13 用户反馈「能看到积分余额，但不知道什么时间过期」——原来只在末尾
	// 打一个「套餐到期 12-12」，赠币桶的到期时间完全没露出来。实测三个桶到期时间
	// 差得很远（套餐 12-12、新用户赠币次年 02-10、每日签到赠币**当天 23:59**），
	// 只显示一个日期等于没说。现在每个桶各自带到期时间。
	var parts []string
	var expiry string
	planName := ""
	for _, s := range j.Skus {
		if s.SkuName != "" {
			planName = s.SkuName
		}
		for _, q := range s.Quotas {
			maxV, curV := parseHWNum(q.SkuValue), parseHWNum(q.Current)
			if maxV <= 0 {
				continue
			}
			switch q.SkuAttrCode {
			case "officeace_points":
				parts = append(parts, fmt.Sprintf("%d/%d 积分", maxV-curV, maxV))
			case "online_search_count":
				parts = append(parts, fmt.Sprintf("联网 %d/%d 次", maxV-curV, maxV))
			}
		}
		if t, err := time.Parse(time.RFC3339Nano, s.Expired); err == nil {
			expiry = t.Local().Format("01-02")
		}
	}

	// 赠币桶：剩余 = points - current_value（current_value 是已用的小数）。
	// 每个桶都带自己的到期时间——「每日签到有礼」是**当天 23:59 失效**的活动赠币，
	// 不标出来用户会以为能攒着以后用（实测 2026-11-13 的签到桶 expired=当日 15:59:59Z）。
	var bonusParts []string
	for _, b := range j.BonusSkus {
		total := parseHWNum(b.Points)
		if total <= 0 {
			continue
		}
		used := parseHWFloat(b.Current)
		left := float64(total) - used
		if left < 0 {
			left = 0
		}
		name := b.ActivityName
		if name == "" {
			name = "赠币"
		}
		label := fmt.Sprintf("%s %.0f", name, left)
		if exp := hwExpiryLabel(b.Expired); exp != "" {
			label += "（" + exp + "）"
		}
		bonusParts = append(bonusParts, label)
	}

	text := "果办"
	if planName != "" {
		text += planName + " "
	}
	text += strings.Join(parts, " · ")
	if len(bonusParts) > 0 {
		text += " ｜ " + strings.Join(bonusParts, "、")
	}
	if expiry != "" {
		text += " · 套餐 " + expiry + " 到期"
	}
	return &creditInfo{OK: true, Text: text, FetchedAt: nowStamp()}
}

// hwExpiryLabel 把 RFC3339 到期时刻转成人话。
//
// 当天到期的要显式标出钟点——果办的「每日签到有礼」赠币是当天 23:59 失效，
// 只写日期（或干脆不写）会让用户以为能攒着用。跨天的只写月-日，省控制台宽度。
func hwExpiryLabel(rfc3339 string) string {
	if strings.TrimSpace(rfc3339) == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		return ""
	}
	local := t.Local()
	now := time.Now()
	if local.Year() == now.Year() && local.YearDay() == now.YearDay() {
		return "今日 " + local.Format("15:04") + " 到期"
	}
	return local.Format("01-02") + " 到期"
}

// parseHWFloat 解析华为的小数字符串（"0.00271542857142857"）。
func parseHWFloat(s string) float64 {
	var f float64
	fmt.Sscanf(strings.TrimSpace(s), "%g", &f)
	return f
}

func parseHWNum(s string) int {
	var n int
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n
}

// hwSubscriptionRaw 额度原始 JSON（网关三要素缓存要用 subscribe_status）。
func hwSubscriptionRaw(cred hwSTS) ([]byte, int, error) {
	return httpGetJSON(hwSubscriptionBase+"/v1/subscription",
		signHuaweiV11(cred, "GET", hwSubscriptionBase+"/v1/subscription", nil, nil))
}

var _ = io.Discard
