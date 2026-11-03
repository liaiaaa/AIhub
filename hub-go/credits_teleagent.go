package main

// TeleAgent（TeleAI）积分查询（2026-11-03）。
//
// 端点前缀是 /superCowork/sapi（**没有 /api/v1**，早期按 /superCowork/sapi/api/v1
// 探测全 404 就是多了这一段）。签名复用 teleagent_proxy.go 的 teleagentSignHeaders，
// GET 请求 body 传 nil 即可。
//
//	GET /superCowork/sapi/user/portal/points/detail
//	    → data.pointsBalance；groups[].active[].{totalQuota,usedQuota,balance,effectiveEnd}
//	GET /superCowork/sapi/user/portal/usage/stats/overview
//	    → data.{taskCount,callCount,pointsUsed}
//
// 实测（2026-11-03）：余额 9688.18、今日消耗 11.82、3 次调用。
// 上游对非官方客户端敏感，这两条都是只读 GET，不产生额外消耗。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const teleagentSAPIBase = "https://agent.teleai.com.cn/superCowork/sapi"

// teleagentPointsPayload points/detail 的响应（只取用得到的字段）。
type teleagentPointsPayload struct {
	Code int `json:"code"`
	Data struct {
		PointsBalance float64 `json:"pointsBalance"`
		UserRole      string  `json:"userRole"`
		Groups        []struct {
			TypeName string `json:"typeName"`
			Active   []struct {
				TotalQuota   float64 `json:"totalQuota"`
				UsedQuota    float64 `json:"usedQuota"`
				Balance      float64 `json:"balance"`
				EffectiveEnd string  `json:"effectiveEnd"`
			} `json:"active"`
		} `json:"groups"`
	} `json:"data"`
}

// teleagentUsageOverview usage/stats/overview 的响应。
type teleagentUsageOverview struct {
	Code int `json:"code"`
	Data struct {
		TaskCount  float64 `json:"taskCount"`
		CallCount  float64 `json:"callCount"`
		PointsUsed float64 `json:"pointsUsed"`
	} `json:"data"`
}

// teleagentCredits 查询 TeleAgent 余额与今日消耗。接进 credits.go 的 fetchCredit。
func teleagentCredits() *creditInfo {
	token, err := teleagentToken()
	if err != nil {
		return &creditInfo{OK: false, Text: "TeleAgent 凭据不可用：" + err.Error()}
	}
	dev, ins, err := teleagentDeviceMeta()
	if err != nil {
		return &creditInfo{OK: false, Text: "TeleAgent 设备信息不可用：" + err.Error()}
	}

	var points teleagentPointsPayload
	if info := teleagentGetJSON(token, dev, ins, "/user/portal/points/detail", &points); info != nil {
		return info
	}
	var overview teleagentUsageOverview
	if info := teleagentGetJSON(token, dev, ins, "/user/portal/usage/stats/overview", &overview); info != nil {
		// 余额拿到了、消耗没拿到：仍然把余额显示出来，别整块失败。
		return &creditInfo{OK: true, Text: teleagentCreditText(points, nil), FetchedAt: nowStamp()}
	}
	return &creditInfo{OK: true, Text: teleagentCreditText(points, &overview), FetchedAt: nowStamp()}
}

// teleagentGetJSON 发一个带签名的只读 GET 并把响应解到 out。
// 返回非 nil 表示这条请求失败（调用方据此降级）。body 最多读 512KB。
func teleagentGetJSON(token, dev, ins, path string, out any) *creditInfo {
	full := path
	if !strings.HasPrefix(full, "/superCowork") {
		full = "/superCowork/sapi" + path
	}
	headers, err := teleagentSignHeaders(token, dev, ins, http.MethodGet, full, nil)
	if err != nil {
		return &creditInfo{OK: false, Text: "签名失败：" + err.Error()}
	}
	req, err := http.NewRequest(http.MethodGet, "https://agent.teleai.com.cn"+full, nil)
	if err != nil {
		return &creditInfo{OK: false, Text: "构造请求失败"}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 必须走 backendClient：裸 http.Client 会无条件相信环境变量里的代理，
	// 代理一死看板就全红（见 proxy_util.go backendClient 的注释）。
	cli := backendClient(8 * time.Second)
	resp, err := cli.Do(req)
	if err != nil {
		return &creditInfo{OK: false, Text: "查询失败（网络错误）"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &creditInfo{OK: false, Text: "TeleAgent 未登录或令牌失效"}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return &creditInfo{OK: false, Text: "上游额度接口不可用（可能改版）"}
	case resp.StatusCode != http.StatusOK:
		return &creditInfo{OK: false, Text: fmt.Sprintf("查询失败（HTTP %d）", resp.StatusCode)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &creditInfo{OK: false, Text: "响应解析失败"}
	}
	return nil
}

// teleagentCreditText 生成额度面板文案。overview 为 nil 表示消耗没查到。
//
// 例：`余额 9688.18 积分 · 今日消耗 11.82（3 次调用）· 最近到期 11-30`
func teleagentCreditText(points teleagentPointsPayload, overview *teleagentUsageOverview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "余额 %s 积分", teleagentNum(points.Data.PointsBalance))
	if overview != nil {
		fmt.Fprintf(&b, " · 今日消耗 %s（%s 次调用）",
			teleagentNum(overview.Data.PointsUsed), teleagentCount(overview.Data.CallCount))
	}
	if end := teleagentEarliestExpiry(points); end != "" {
		fmt.Fprintf(&b, " · 最近到期 %s", end)
	}
	return b.String()
}

// teleagentEarliestExpiry 找出各活动积分包里最早到期的那个，格式化成 MM-DD。
// 上游格式 `2026-11-30 23:59:59`；解析失败就原样返回空串（不猜）。
func teleagentEarliestExpiry(points teleagentPointsPayload) string {
	earliest := time.Time{}
	for _, g := range points.Data.Groups {
		for _, a := range g.Active {
			s := strings.TrimSpace(a.EffectiveEnd)
			if s == "" {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
			if err != nil {
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
		}
	}
	if earliest.IsZero() {
		return ""
	}
	return earliest.Format("01-02")
}

// teleagentNum 金额格式化：整数不带小数，其余保留原精度（9688.18 → "9688.18"）。
func teleagentNum(v float64) string {
	if math.Abs(v-math.Round(v)) < 1e-9 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// teleagentCount 次数格式化（2 → "2"）。
func teleagentCount(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64)
}

func nowStamp() string {
	return time.Now().Format("15:04:05")
}
