package main

// 套餐额度查询（2026-09-27 实测打通，健康面板用）：
//
// GET https://zcode.z.ai/api/v1/zcode-plan/billing/balance
// 需要 Authorization Bearer <zcodejwttoken> + X-Device-Mid +
// x-coding-plan-api-key 三头齐备（探针 D 实测 HTTP 200），返回
// data.plans[]（订阅计划）与 data.balances[]（分桶余额，remaining_units）。
// 这是直连可读的套餐额度视图：只读、不碰验证码、不消耗额度。
// 401 时重读桌面端凭据（JWT 会随桌面端重新登录更新）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// zcodeQuotaFetchInterval 额度刷新节流。
const zcodeQuotaFetchInterval = 5 * time.Minute

// zcodeQuotaBucket 一个余额桶（对齐 data.balances[] 字段）。
type zcodeQuotaBucket struct {
	PlanName       string   `json:"plan_name"`
	ShowName       string   `json:"show_name"`
	Capabilities   []string `json:"capabilities,omitempty"`
	TotalUnits     int64    `json:"total_units"`
	UsedUnits      int64    `json:"used_units"`
	RemainingUnits int64    `json:"remaining_units"`
	UnitType       string   `json:"unit_type,omitempty"`
	ExpiresAt      int64    `json:"expires_at,omitempty"`
}

// quotaSnapshot 返回缓存的额度视图；无缓存时现场拉一次（3s 超时）。
// 失败返回 nil + 错误信息（health 面板降级展示，不影响其他字段）。
func (p *ZcodeProxy) quotaSnapshot() ([]zcodeQuotaBucket, string) {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	now := time.Now()
	if p.quotaFetchedAt.IsZero() || now.Sub(p.quotaFetchedAt) >= zcodeQuotaFetchInterval {
		p.refreshQuotaLocked()
	}
	return p.quotaBuckets, p.quotaError
}

// quotaCachedForStatus 非阻塞版：只读缓存（可能为 nil），过期则后台异步
// 刷新——控制台状态轮询高频，绝不能被 3s 的计费请求卡住。
func (p *ZcodeProxy) quotaCachedForStatus() []zcodeQuotaBucket {
	p.quotaMu.Lock()
	stale := p.quotaFetchedAt.IsZero() || time.Since(p.quotaFetchedAt) >= zcodeQuotaFetchInterval
	var buckets []zcodeQuotaBucket
	if !stale {
		buckets = p.quotaBuckets
	}
	p.quotaMu.Unlock()
	if stale {
		go p.quotaSnapshot() // 后台补拉，下次轮询可见
	}
	return buckets
}

// zcodeStatusDetail 控制台 Zcode 卡片的状态行：通道形态 + 剩余额度摘要。
func zcodeStatusDetail(p *ZcodeProxy) string {
	models := len(zcodeFilterCatalog(p.catalogSnapshot()))
	if wait, open := p.captchaBreakerOpen(); open {
		return fmt.Sprintf("验证码熔断中（%d 秒后自动恢复）· IP 风控惩罚期，也可重启光猫立即恢复", int(wait.Seconds())+1)
	}
	if _, _, relay := p.relayTarget(); !relay {
		return fmt.Sprintf("Z.ai Bearer 直连 · %d 个模型", models)
	}
	detail := fmt.Sprintf("免费额度中继 · %d 个模型", models)
	buckets := p.quotaCachedForStatus()
	if len(buckets) == 0 {
		return detail
	}
	// 剩余额度降序取前 3 桶，中文单位摘要（Flash 剩2.5亿 / 5.3 剩300万）
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].RemainingUnits > buckets[j].RemainingUnits })
	parts := make([]string, 0, 3)
	for i, b := range buckets {
		if i >= 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s 剩%s", b.ShowName, zcodeHumanizeCN(b.RemainingUnits)))
	}
	return detail + " · " + strings.Join(parts, " / ")
}

// zcodeHumanizeCN 中文计数单位（亿/万）。
func zcodeHumanizeCN(n int64) string {
	switch {
	case n >= 100_000_000:
		v := float64(n) / 100_000_000
		if v == float64(int64(v)) {
			return fmt.Sprintf("%.0f亿", v)
		}
		return fmt.Sprintf("%.1f亿", v)
	case n >= 10_000:
		return fmt.Sprintf("%.0f万", float64(n)/10_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// refreshQuotaLocked 拉取 billing/balance（调用方持 quotaMu）。
func (p *ZcodeProxy) refreshQuotaLocked() {
	p.quotaError = ""
	p.quotaFetchedAt = time.Now()

	if p.auth == nil || p.identity == nil {
		p.quotaError = "无桌面端凭据"
		return
	}
	jwt := p.auth.JWT()
	if jwt == "" {
		p.quotaError = "凭据中无 zcodejwttoken"
		return
	}

	url := fmt.Sprintf("%s/api/v1/zcode-plan/billing/balance?app_version=%s&platform=%s",
		zcodeCatalogOrigin, p.identity.appVersion, p.identity.platformParam())
	client := &http.Client{Transport: directClient().Transport, Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		p.quotaError = err.Error()
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("x-coding-plan-api-key", p.apiKey)
	setIdentityHeaders(req, p.identity.contextHeaders()) // 含 X-Device-Mid

	resp, err := client.Do(req)
	if err != nil {
		p.quotaError = err.Error()
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.quotaError = fmt.Sprintf("billing/balance HTTP %d", resp.StatusCode)
		return
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plans []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				PlanID string `json:"plan_id"`
			} `json:"plans"`
			Balances []struct {
				PlanID         string   `json:"plan_id"`
				ShowName       string   `json:"show_name"`
				Capabilities   []string `json:"capabilities"`
				TotalUnits     int64    `json:"total_units"`
				UsedUnits      int64    `json:"used_units"`
				RemainingUnits int64    `json:"remaining_units"`
				UnitType       string   `json:"unit_type"`
				ExpiresAt      int64    `json:"expires_at"`
			} `json:"balances"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		p.quotaError = err.Error()
		return
	}
	if parsed.Code != 0 {
		p.quotaError = fmt.Sprintf("billing/balance code=%d msg=%s", parsed.Code, parsed.Msg)
		return
	}
	planNames := make(map[string]string, len(parsed.Data.Plans))
	for _, pl := range parsed.Data.Plans {
		planNames[pl.PlanID] = pl.Name
	}
	buckets := make([]zcodeQuotaBucket, 0, len(parsed.Data.Balances))
	for _, b := range parsed.Data.Balances {
		buckets = append(buckets, zcodeQuotaBucket{
			PlanName:       planNames[b.PlanID],
			ShowName:       b.ShowName,
			Capabilities:   b.Capabilities,
			TotalUnits:     b.TotalUnits,
			UsedUnits:      b.UsedUnits,
			RemainingUnits: b.RemainingUnits,
			UnitType:       b.UnitType,
			ExpiresAt:      b.ExpiresAt,
		})
	}
	p.quotaBuckets = buckets
}
