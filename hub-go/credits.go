package main

// 平台剩余额度查询（控制台额度面板）。全部只读、幂等，5 分钟缓存，
// 过期后台补拉——控制台轮询绝不被上游请求阻塞。
//
// 接口证据（2026-10-07 开源源码逐仓核对）：
//   - Qoder CN：GET openapi.qoder.com.cn/sash/api/v2/me/usage
//     （Bearer dt- 设备令牌 + Cosy-ClientType:10 + UA "Qoder"；
//     余额 = qoderUsage.userQuota.remaining + addOnQuota.remaining；
//     证据 yetone/magpie internal/qoder/usage.go、TraeWorkAssistant qoder_credits.rs）
//   - WorkBuddy 国内：POST copilot.tencent.com/billing/meter/get-user-resource-summary
//     （body {}；UA 必须 WorkBuddy/<ver>，网关对陌生 UA 回业务码 10085；
//     余额 = Σ data.Packages[].CycleRemainCapacity；
//     备用老接口 www.codebuddy.cn/v2/billing/meter/get-user-resource，
//     必带头 X-CodeBuddy-Request:1，双层信封 data.Response.Data.Accounts[]）
//   - WorkBuddy 国际：同族接口打 www.workbuddy.ai（billing 路径无 /v2 前缀），
//     UA 平台段须含 "WorkBuddy AI"，送错平台段可能触发 11140 风控
//     （证据 workbuddy2api-panel internal/upstream/client.go L714-763）
//   - Trae CN：POST api.trae.cn/trae/api/v2/pay/ide_user_ent_usage（body {}，
//     Cloud-IDE-JWT + X-User-Region:CN + X-Device-Id；
//     remain = Σ(credits_limit - usage.credits_amount)，limit<=0 的包跳过；
//     证据 trae2api-web cmd/credit/main.go）
// Zcode 余额不在这里——zcode_quota.go 已实现并展示在状态详情里。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// creditInfo 单平台额度快照（控制台展示用）。
type creditInfo struct {
	OK        bool   `json:"ok"`
	Text      string `json:"text"`
	FetchedAt string `json:"fetched_at,omitempty"`
	// Diff 是「较前一日」的余额变化文案（仅国际版填，见 credit_history.go）。
	// 用于回答「昨天上报的活跃，30 分到账了吗」——不必去官网翻积分明细。
	Diff string `json:"diff,omitempty"`
	// Burn/Burned：国际版消耗预警（2026-10-13，见 intl_burn.go）。
	// Burned=true 表示当日监测到余额下探（标 0.00x 的免费模型在扣积分），
	// Burn 为当日累计扣费额。前端显示黄牌提醒用户停手。
	Burn   float64 `json:"burn,omitempty"`
	Burned bool    `json:"burned,omitempty"`
}

type creditEntry struct {
	mu        sync.Mutex
	info      *creditInfo
	fetchedAt time.Time
	fetching  bool
}

var creditsState = struct {
	mu      sync.Mutex
	entries map[string]*creditEntry
}{entries: map[string]*creditEntry{}}

const creditsTTL = 5 * time.Minute

var creditKeys = []string{"dom", "intl", "qoder", "trae"}

// creditsSnapshot 返回各平台额度（缓存优先）；过期/强制时后台补拉。
// force=true 立即作废全部缓存。
func creditsSnapshot(h *Hub, force bool) map[string]creditInfo {
	if force {
		creditsState.mu.Lock()
		for _, e := range creditsState.entries {
			e.mu.Lock()
			e.fetchedAt = time.Time{}
			e.mu.Unlock()
		}
		creditsState.mu.Unlock()
	}
	out := map[string]creditInfo{}
	now := time.Now()
	for _, key := range creditKeys {
		creditsState.mu.Lock()
		e := creditsState.entries[key]
		if e == nil {
			e = &creditEntry{}
			creditsState.entries[key] = e
		}
		creditsState.mu.Unlock()

		e.mu.Lock()
		stale := e.info == nil || now.Sub(e.fetchedAt) >= creditsTTL
		if stale && !e.fetching {
			e.fetching = true
			go func(k string, en *creditEntry) {
				defer recoverGoroutine("credits.go:94")
				info := fetchCredit(h, k)
				en.mu.Lock()
				en.info = info
				en.fetchedAt = time.Now()
				en.fetching = false
				en.mu.Unlock()
			}(key, e)
		}
		if e.info != nil {
			out[key] = *e.info
		} else {
			out[key] = creditInfo{OK: false, Text: "查询中…"}
		}
		e.mu.Unlock()
	}
	return out
}

// creditsOne 返回单个平台额度（缓存优先）；force=true 立即作废该平台缓存。
// 用于控制台「每个瓦片独立刷新额度」——不触碰其他平台，未登录的平台不会被连带查询。
func creditsOne(h *Hub, key string, force bool) (creditInfo, bool) {
	valid := false
	for _, k := range creditKeys {
		if k == key {
			valid = true
			break
		}
	}
	if !valid {
		return creditInfo{OK: false, Text: "未知平台"}, false
	}
	creditsState.mu.Lock()
	e := creditsState.entries[key]
	if e == nil {
		e = &creditEntry{}
		creditsState.entries[key] = e
	}
	creditsState.mu.Unlock()
	if force {
		e.mu.Lock()
		e.fetchedAt = time.Time{}
		e.mu.Unlock()
	}
	now := time.Now()
	e.mu.Lock()
	stale := e.info == nil || now.Sub(e.fetchedAt) >= creditsTTL
	if stale && !e.fetching {
		e.fetching = true
		go func(k string, en *creditEntry) {
			defer recoverGoroutine("credits.go:143")
			info := fetchCredit(h, k)
			en.mu.Lock()
			en.info = info
			en.fetchedAt = time.Now()
			en.fetching = false
			en.mu.Unlock()
		}(key, e)
	}
	if e.info != nil {
		info := *e.info
		e.mu.Unlock()
		return info, true
	}
	e.mu.Unlock()
	return creditInfo{OK: false, Text: "查询中…"}, true
}

func fetchCredit(h *Hub, key string) *creditInfo {
	switch key {
	case "dom":
		return wbCredits(h, false)
	case "intl":
		return wbCredits(h, true)
	case "qoder":
		return qoderCredits()
	case "trae":
		return traeCredits()
	}
	return &creditInfo{OK: false, Text: "未知平台"}
}

// ---------- WorkBuddy 国内/国际 ----------

func wbCredits(h *Hub, intl bool) *creditInfo {
	var cm *CredentialManager
	base := "https://copilot.tencent.com"
	ua := officialDomBillingUA() // 单段：billing 域白名单 UA
	name := "国内 WorkBuddy"
	if !intl {
		h.mu.Lock()
		dir := domCredDir(h.domCred)
		h.mu.Unlock()
		p := NewDomProxyDir(dir)
		path := p.AuthFilePath()
		if path == "" || !fileExists(path) {
			return &creditInfo{OK: false, Text: "未找到国内凭据"}
		}
		cm = NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	} else {
		h.mu.Lock()
		cred := h.intlCred
		h.mu.Unlock()
		dir := credDirs()[cred]
		if dir == "" {
			return &creditInfo{OK: false, Text: "未选择国际凭证"}
		}
		p := NewIntlProxy(dir)
		path := p.AuthFilePath()
		if path == "" || !fileExists(path) {
			return &creditInfo{OK: false, Text: "未找到国际凭据"}
		}
		cm = NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
		base = "https://www.workbuddy.ai"
		ua = officialIntlUA() // 平台段必须是 WorkBuddy AI
		name = "国际 WorkBuddy"
	}
	headers, err := cm.Headers()
	if err != nil {
		return &creditInfo{OK: false, Text: name + "凭据读取失败"}
	}
	// 国际版要按 uid 记每日快照（凭证目录名会变，uid 才是稳定主键）。
	// 昵称一并取上，快照里存昵称才认得出哪个账号是哪个。
	uid, nick := "", ""
	if intl {
		if sum, serr := cm.Summary(); serr == nil {
			uid, _ = sum["uid"].(string)
			nick, _ = sum["nickname"].(string)
		}
	}
	headers["User-Agent"] = ua
	headers["X-Product"] = "SaaS"
	headers["X-IDE-Type"] = "WorkBuddy"

	// 主接口：get-user-resource-summary（国内外同构，billing 路径均无 /v2）
	body, status, err := httpPostJSON(base+"/billing/meter/get-user-resource-summary", headers, []byte("{}"))
	if err == nil && status == http.StatusOK {
		if remain, total, ok := wbSummaryParse(body); ok {
			return wbCreditResult(intl, uid, nick, remain, total)
		}
	}
	// 备用老接口：get-user-resource（国内走 /v2；国际无 /v2 首选、/v2 兜底）
	paths := []string{"/v2/billing/meter/get-user-resource"}
	if !intl {
		base = "https://www.codebuddy.cn"
	} else {
		paths = []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"}
	}
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca", "Status": []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02") + " 00:00:00",
		"PackageEndTimeRangeEnd":   "2126-01-01 00:00:00",
	})
	for _, path := range paths {
		headers["X-CodeBuddy-Request"] = "1"
		body, status, err = httpPostJSON(base+path, headers, payload)
		if err != nil || status != http.StatusOK {
			continue
		}
		if remain, total, ok := wbResourceParse(body); ok {
			return wbCreditResult(intl, uid, nick, remain, total)
		}
	}
	return &creditInfo{OK: false, Text: name + "额度查询失败（HTTP " + fmt.Sprintf("%d", status) + "，若为 200 则是响应字段变更）"}
}

// flexNum 容量字段弹性数值：实测上游把这些字段发成 JSON 字符串（"30"），
// 数字与字符串都要能接。
type flexNum float64

func (n *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*n = 0 // 非数值容错为 0，不让整包解析失败
		return nil
	}
	*n = flexNum(f)
	return nil
}

// wbCreditResult 组装额度结果；国际版额外记录每日快照并附上差额文案。
func wbCreditResult(intl bool, uid, name string, remain, total float64) *creditInfo {
	info := &creditInfo{
		OK:        true,
		Text:      "余额 " + fmtCN(remain) + " / " + fmtCN(total) + " 积分",
		FetchedAt: time.Now().Format("15:04"),
	}
	if intl && uid != "" {
		creditHistoryRecord(uid, name, remain, total)
		info.Diff = creditHistoryDiffText(uid)
		if burn := recordIntlCreditWater(uid, remain); burn > 0.005 {
			info.Burn = burn
			info.Burned = true
		}
	}
	return info
}

// wbSummaryParse 新接口解析：data.Packages[].CycleRemainCapacity / CycleTotalCapacity
// （字段名 PascalCase、值为字符串，靠 json 大小写不敏感匹配 + flexNum 接）。
func wbSummaryParse(body []byte) (remain, total float64, ok bool) {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Packages []struct {
				CycleRemainCapacity flexNum `json:"cycleRemainCapacity"`
				CycleTotalCapacity  flexNum `json:"cycleTotalCapacity"`
			} `json:"packages"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Code != 0 || len(out.Data.Packages) == 0 {
		return 0, 0, false
	}
	for _, p := range out.Data.Packages {
		remain += float64(p.CycleRemainCapacity)
		total += float64(p.CycleTotalCapacity)
	}
	return remain, total, true
}

// wbResourceParse 老接口解析：外层 {code,data} → data.Response.Data.Accounts[]
// （双层信封，实测踩坑点）；精确字段优先。
func wbResourceParse(body []byte) (remain, total float64, ok bool) {
	var out struct {
		Code int `json:"code"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []struct {
						CapacityRemain             flexNum `json:"capacityRemain"`
						CapacitySize               flexNum `json:"capacitySize"`
						CycleCapacityRemain        flexNum `json:"cycleCapacityRemain"`
						CycleCapacitySize          flexNum `json:"cycleCapacitySize"`
						CycleCapacityRemainPrecise flexNum `json:"cycleCapacityRemainPrecise"`
						CycleCapacitySizePrecise   flexNum `json:"cycleCapacitySizePrecise"`
					} `json:"accounts"`
				} `json:"data"`
			} `json:"response"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Code != 0 || len(out.Data.Response.Data.Accounts) == 0 {
		return 0, 0, false
	}
	for _, a := range out.Data.Response.Data.Accounts {
		r, t := float64(a.CycleCapacityRemain), float64(a.CycleCapacitySize)
		if a.CycleCapacityRemainPrecise != 0 || a.CycleCapacitySizePrecise != 0 {
			r, t = float64(a.CycleCapacityRemainPrecise), float64(a.CycleCapacitySizePrecise)
		} else if a.CapacityRemain != 0 || a.CapacitySize != 0 {
			r, t = float64(a.CapacityRemain), float64(a.CapacitySize)
		}
		remain += r
		total += t
	}
	return remain, total, true
}

// ---------- Qoder ----------

// qoderUsageQuota Qoder 额度对象（camelCase/snake_case 双字段之一，json 反序列化
// 本身大小写不敏感）。
type qoderUsageQuota struct {
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
}

// qoderUsagePayload /sash/api/v2/me/usage 的 qoderUsage 信封体（裸形态同构）。
type qoderUsagePayload struct {
	UserQuota       qoderUsageQuota  `json:"userQuota"`
	AddOnQuota      *qoderUsageQuota `json:"addOnQuota"`
	IsQuotaExceeded bool             `json:"isQuotaExceeded"`
	ExpiresAt       int64            `json:"expiresAt"`
}

func qoderCredits() *creditInfo {
	token, err := qoderDeviceToken()
	if err != nil {
		return &creditInfo{OK: false, Text: "Qoder 凭据不可用"}
	}
	headers := map[string]string{
		"Authorization":   "Bearer " + token,
		"Cosy-ClientType": "10",
		"User-Agent":      "Qoder",
		"Accept":          "application/json",
	}
	body, status, err := httpGetJSON(qoderOpenAPIBase+"/sash/api/v2/me/usage", headers)
	if err != nil || status != http.StatusOK {
		return &creditInfo{OK: false, Text: fmt.Sprintf("Qoder 额度查询失败（HTTP %d）", status)}
	}
	// 两种形态：displayMode 信封 {"qoderUsage":{...}}（桌面端同款）与裸顶层
	var envelope struct {
		QoderUsage *qoderUsagePayload `json:"qoderUsage"`
	}
	var usage *qoderUsagePayload
	if json.Unmarshal(body, &envelope) == nil && envelope.QoderUsage != nil {
		usage = envelope.QoderUsage
	} else {
		var bare qoderUsagePayload
		if json.Unmarshal(body, &bare) == nil {
			usage = &bare
		}
	}
	if usage == nil {
		return &creditInfo{OK: false, Text: "Qoder 响应里没有额度数据"}
	}
	remain := usage.UserQuota.Remaining
	total := usage.UserQuota.Total
	if usage.AddOnQuota != nil {
		remain += usage.AddOnQuota.Remaining
		total += usage.AddOnQuota.Total
	}
	if usage.ExpiresAt > 0 && time.Now().UnixMilli() > usage.ExpiresAt {
		return &creditInfo{OK: true, Text: "额度已过期（剩 " + fmtCN(remain) + "）", FetchedAt: time.Now().Format("15:04")}
	}
	return &creditInfo{OK: true, Text: "余额 " + fmtCN(remain) + " / " + fmtCN(total) + " Credits", FetchedAt: time.Now().Format("15:04")}
}

// ---------- Trae ----------

func traeCredits() *creditInfo {
	info, dev, err := traeReadAuth()
	if err != nil {
		return &creditInfo{OK: false, Text: "Trae 凭据不可用"}
	}
	headers := map[string]string{
		"Authorization": "Cloud-IDE-JWT " + info.Token,
		"X-User-Region": "CN",
		"X-Device-Id":   dev.DeviceID,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"User-Agent":    "Trae/" + traeIdeVersion,
	}
	body, status, err := httpPostJSON(traeUgHost+"/trae/api/v2/pay/ide_user_ent_usage", headers, []byte("{}"))
	if err != nil || status != http.StatusOK {
		// 2026-10-19：401 几乎总是登录态过期，而原来只报「HTTP 401」，
		// 用户不知道是哪里坏了。这里把 JWT 的过期时刻解出来，直接说清楚。
		// 实测（2026-10-19 15:09）：token iat=10-05 14:14、exp=10-19 14:14，
		// 过期 54 分钟后查询返回 401 + {"code":1001,"message":"not able to authenticate you"}。
		if status == http.StatusUnauthorized {
			if exp, ok := jwtExpiry(info.Token); ok {
				return &creditInfo{OK: false, Text: fmt.Sprintf(
					"Trae 登录态已过期（%s 到期），请在 Trae 桌面端重新登录后重启该线路",
					exp.Format("01-02 15:04"))}
			}
			return &creditInfo{OK: false, Text: "Trae 登录态已过期，请在 Trae 桌面端重新登录后重启该线路"}
		}
		return &creditInfo{OK: false, Text: fmt.Sprintf("Trae 额度查询失败（HTTP %d）", status)}
	}
	var env struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if json.Unmarshal(body, &env) != nil {
		return &creditInfo{OK: false, Text: "Trae 响应解析失败"}
	}
	var remain, total float64
	for _, p := range env.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		total += l
		remain += l - p.Usage.CreditsAmount
	}
	if total == 0 {
		return &creditInfo{OK: true, Text: "暂无额度包", FetchedAt: time.Now().Format("15:04")}
	}
	return &creditInfo{OK: true, Text: "余额 " + fmtCN(remain) + " / " + fmtCN(total) + " 积分", FetchedAt: time.Now().Format("15:04")}
}

// ---------- 展示工具 ----------

// fmtCN 千分位整数（积分数值展示）。
func fmtCN(f float64) string {
	n := int64(f + 0.5)
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}

// jwtExpiry 解出 JWT 载荷里的 exp（不验签，只读声明）。
// 用途：401 时告诉用户「登录态什么时候到期的」，而不是干巴巴一个 HTTP 401。
// 解不出来返回 ok=false —— 调用方要能退回到不带时间的文案。
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}
