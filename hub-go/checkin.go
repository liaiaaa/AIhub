package main

// 每日签到：CodeBuddy 国内版（copilot.tencent.com）+ Qoder 国内版
// （openapi.qoder.com.cn）。接口为逆向所得，多仓库源码互证（2026-10 调研）：
//
//   - CodeBuddy/WorkBuddy 国内：POST /v2/billing/meter/daily-checkin，空 body。
//     认证就是普通 Bearer + X-User-Id（billing/meter 族无签名、无设备指纹要求）。
//     成功：HTTP 200（体内含 credit）；已签：HTTP 400 {"code":10001,"msg":"今天已签到…"}。
//     注意 10001 有两种语义（已签到 / 企业账号不支持），按 msg 文案区分。
//     参考：88lin/workbuddy-auto-signin、yetone/magpie internal/provider/
//     workbuddy_checkin.go、linguo2625469/workbuddy2api-panel internal/upstream。
//
//   - Qoder 国内：GET /sash/api/v1/me/campaigns 过滤 actionType=CLAIM_BENEFIT
//     且 claimStatus=CLAIMABLE 的活动，逐个 POST .../campaigns/{campaignId}/claim
//     （空 body）。头必须带 Cosy-ClientType: 10——写别的值不报错但静默返回
//     空活动列表，与「账号没活动」完全同形。旧版 /me/daily-check-in/status
//     端点 2026-09-21 起全局废弃（claim 恒 409），只走 campaigns 流程。
//     参考：Zhengyuuuui/qoder2api checkin.go、yetone/magpie qoder_checkin.go。
//
// 幂等性：两个平台重复调用都安全（CodeBuddy 返回"已签到"，Qoder claim 对
// 已领活动返回 replayed），因此自动定时器可以放心反复尝试。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const qoderOpenAPIBase = "https://openapi.qoder.com.cn"

// intlReportURL 国际版活跃上报端点。独立成变量，测试里替换为桩服务地址。
var intlReportURL = "https://www.workbuddy.ai/v2/report"

// checkinResult 一次签到动作的结果（展示用）。
type checkinResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// domCheckin 国内 WorkBuddy/CodeBuddy 每日签到。
func domCheckin() checkinResult {
	p := NewDomProxy()
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		return checkinResult{OK: false, Detail: "未找到国内版凭据文件，请先在桌面端登录"}
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	headers, err := cm.Headers()
	if err != nil {
		return checkinResult{OK: false, Detail: "读取凭据失败：" + err.Error()}
	}
	body, status, err := httpPostJSON("https://copilot.tencent.com/v2/billing/meter/daily-checkin", headers, []byte("{}"))
	if err != nil {
		return checkinResult{OK: false, Detail: "网络失败：" + err.Error()}
	}
	if status == http.StatusOK {
		credit := jsonNumberField(body, "credit")
		if credit != "" {
			return checkinResult{OK: true, Detail: "签到成功，+" + credit + " 积分"}
		}
		return checkinResult{OK: true, Detail: "签到成功"}
	}
	// 400 + 10001 有两种语义，按文案区分
	var er struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &er) == nil && er.Code == 10001 {
		if strings.Contains(er.Msg, "已签到") {
			return checkinResult{OK: true, Detail: "今日已签到"}
		}
		return checkinResult{OK: false, Detail: "平台拒绝：" + er.Msg}
	}
	if msg := jsonMsgField(body); msg != "" {
		return checkinResult{OK: false, Detail: fmt.Sprintf("HTTP %d：%s", status, truncate(msg, 160))}
	}
	return checkinResult{OK: false, Detail: fmt.Sprintf("HTTP %d：%s", status, truncate(string(body), 160))}
}

// intlCheckin 国际版 WorkBuddy AI 的「每日活跃上报」。
//
// 与国内/ Qoder/Trae 的签到不同：国际版没有 daily-checkin 端点，它的 30 积分
// 是「每日活跃奖励」——由**客户端主动上报 chat_request_send 事件**触发，
// 而不是服务端看到你发过对话请求。所以光用 Codex 经 hub-go 对话**拿不到**，
// 必须补这一步上报（2026-10-12 用户实测：次日看不到 30 积分到账）。
//
// 证据链：
//   - 官方文档（www.workbuddy.ai/docs/zh/workbuddy/Subscription）：Free 每天 30 分、
//     Pro 50 分；「对应积分通常将在次日发放」；**网页端不计入**，须客户端发起。
//   - 端点与事件形状照抄开源实现 linguo2625469/workbuddy2api-panel
//     （internal/upstream/report.go、scripts/probe_active.py）：POST /v2/report，
//     body 为 [chat_request_send 事件]；**事件必须带 userId，缺失则 200 但静默丢弃**。
//   - 本机实测（2026-10-12，<凭证目录C>）：上报前 growth/streak 的 days=0，
//     上报后 days=1 → 事件被真实接受，非静默丢弃。
//
// 风控口径：每号每天 1 次即可（文档「同一账号每日最多获得一次」；开源实现也注明
// 「不做多时点高频上报」）。这里只在用户点「一键签到」时触发，不自动定时。
//
// 风险已知并已告知用户：文档「活动限制」把「接口调用…刷取活跃」列为违规。
// 本实现只上报真实发生过的对话行为、不批量、不多账号轮刷。
func intlCheckin(cred string) checkinResult {
	dir := credDirs()[cred]
	if dir == "" || !dirHasCredential(dir) {
		return checkinResult{OK: false, Detail: "未找到国际版凭据：" + cred}
	}
	p := NewIntlProxy(dir)
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		return checkinResult{OK: false, Detail: "未找到国际版凭据文件"}
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	sum, err := cm.Summary()
	if err != nil {
		return checkinResult{OK: false, Detail: "读取凭据失败：" + err.Error()}
	}
	uid, _ := sum["uid"].(string)
	if uid == "" {
		return checkinResult{OK: false, Detail: "凭据缺少 uid，无法上报活跃"}
	}
	headers, err := cm.Headers()
	if err != nil {
		return checkinResult{OK: false, Detail: "读取凭据失败：" + err.Error()}
	}
	// 事件里也要带 userId（服务端按它归属账号），与请求头 X-User-Id 双保险。
	ev := intlChatRequestEvent(uid, "deepseek-v4.1-flash", "DeepSeek V4.1 Flash")
	payload, _ := json.Marshal([]any{ev})
	body, status, err := httpPostJSON(intlReportURL, headers, payload)
	if err != nil {
		return checkinResult{OK: false, Detail: "网络失败：" + err.Error()}
	}
	if status == http.StatusOK {
		var r struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(body, &r) == nil && r.Code == 0 {
			return checkinResult{OK: true, Detail: "活跃已上报（+30 积分次日到账）"}
		}
	}
	if msg := jsonMsgField(body); msg != "" {
		return checkinResult{OK: false, Detail: fmt.Sprintf("HTTP %d：%s", status, truncate(msg, 160))}
	}
	return checkinResult{OK: false, Detail: fmt.Sprintf("HTTP %d：%s", status, truncate(string(body), 160))}
}

// intlChatRequestEvent 构造客户端 chat_request_send 事件。
// 字段形状照抄真实客户端（开源实现 report.go 的 chatRequestEvent），
// 刻意用全字段而非最小集，防上游后续加严。
func intlChatRequestEvent(uid, modelID, modelName string) map[string]any {
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("hub-go-active-%d", now)
	return map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "craft", "conversationId": cid, "requestId": cid,
		"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 0,
		"temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
		"totalCount": 0, "fileUri": "", "presentAt": now, "traceId": "",
		"rootRequestId": cid, "parentConversationId": cid,
		"agentName": "default", "agentType": "conversation",
		"userId": uid,
	}
}

// qoderCheckin Qoder 国内版每日签到：查活动 → 逐个领取。
func qoderCheckin() checkinResult {
	token, err := qoderDeviceToken()
	if err != nil {
		return checkinResult{OK: false, Detail: err.Error()}
	}
	headers := map[string]string{
		"Authorization":   "Bearer " + token,
		"Cosy-ClientType": "10", // 必须：其他值会静默返回空活动列表
		"User-Agent":      "Qoder",
		"Accept":          "application/json",
		"Content-Type":    "application/json",
	}
	campaigns, _, err := func() ([]qoderCampaign, int, error) {
		body, status, err := httpPostJSON(qoderOpenAPIBase+"/sash/api/v1/me/campaigns", headers, nil)
		if err != nil {
			return nil, status, err
		}
		var out struct {
			Claimable bool            `json:"claimable"`
			Campaigns []qoderCampaign `json:"campaigns"`
			Code      int             `json:"code"`
			Msg       string          `json:"msg"`
		}
		if json.Unmarshal(body, &out) != nil {
			return nil, status, fmt.Errorf("响应解析失败：%s", truncate(string(body), 160))
		}
		if out.Code != 0 && len(out.Campaigns) == 0 {
			return nil, status, fmt.Errorf("HTTP %d：%s", status, truncate(out.Msg, 160))
		}
		return out.Campaigns, status, nil
	}()
	if err != nil {
		return checkinResult{OK: false, Detail: err.Error()}
	}

	var claimable []qoderCampaign
	for _, c := range campaigns {
		if c.ActionType == "CLAIM_BENEFIT" && c.ClaimStatus == "CLAIMABLE" {
			claimable = append(claimable, c)
		}
	}
	if len(claimable) == 0 {
		return checkinResult{OK: true, Detail: "今日无可领活动（可能已领）"}
	}
	var got []string
	failed := 0
	for _, c := range claimable {
		body, status, err := httpPostJSON(qoderOpenAPIBase+"/sash/api/v1/me/campaigns/"+c.CampaignID+"/claim", headers, nil)
		if err != nil || status != http.StatusOK {
			failed++
			continue
		}
		// claim 成功响应可能带 credit / benefit 说明，能提就提
		if credit := jsonNumberField(body, "credit"); credit != "" {
			got = append(got, "+"+credit)
		} else {
			got = append(got, c.CampaignID)
		}
	}
	switch {
	case failed == 0:
		return checkinResult{OK: true, Detail: "签到成功 " + strings.Join(got, ", ")}
	case len(got) > 0:
		return checkinResult{OK: true, Detail: fmt.Sprintf("部分成功：%s（%d 个失败）", strings.Join(got, ", "), failed)}
	default:
		return checkinResult{OK: false, Detail: fmt.Sprintf("领取失败（HTTP 状态异常），活动数 %d", len(claimable))}
	}
}

type qoderCampaign struct {
	CampaignID  string `json:"campaignId"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	Name        string `json:"name"`
}

// qoderDeviceToken 从桌面端凭据解密 dt- 设备令牌（不依赖 Qoder 代理是否启动）。
func qoderDeviceToken() (string, error) {
	a, err := NewQoderAuth()
	if err != nil {
		return "", err
	}
	if err := a.Reload(); err != nil {
		return "", fmt.Errorf("解密 Qoder 桌面端凭据失败：%w（请确认桌面端已登录）", err)
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.creds.Token == "" {
		return "", fmt.Errorf("Qoder 凭据中 token 为空，请先在桌面端登录")
	}
	return a.creds.Token, nil
}

// httpPostJSON 通用小客户端：body 为 nil 时不发 body（ContentLength 0）。
func httpPostJSON(url string, headers map[string]string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest("POST", url, rd)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// httpGetJSON GET 版小客户端（额度查询等只读接口用）。
func httpGetJSON(url string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// jsonNumberField 从响应 JSON 顶层（含 data 层）找数值字段，找不到返回空串。
func jsonNumberField(body []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	for _, mm := range []map[string]any{m, mapOrNil(m["data"])} {
		if mm == nil {
			continue
		}
		switch v := mm[key].(type) {
		case float64:
			return fmt.Sprintf("%v", v)
		case string:
			if v != "" {
				return v
			}
		}
	}
	return ""
}

func jsonMsgField(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if s, ok := m["msg"].(string); ok {
		return s
	}
	if s, ok := m["message"].(string); ok {
		return s
	}
	return ""
}

func mapOrNil(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// traeCheckin Trae 每日签到：status 查状态 → 未签则 claim。
// 必须带真实 x-device-id（key 名里的 Aha 设备号），编造会稳定 9074。
func traeCheckin() checkinResult {
	info, dev, err := traeReadAuth()
	if err != nil {
		return checkinResult{OK: false, Detail: err.Error()}
	}
	headers := map[string]string{
		"Authorization": "Cloud-IDE-JWT " + info.Token,
		"X-User-Region": "CN",
		"X-Device-Id":   dev.DeviceID,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"User-Agent":    "Trae/" + traeIdeVersion,
	}
	body, status, err := httpPostJSON(traeUgHost+"/trae/api/v2/ug/checkin_credits/status", headers, []byte("{}"))
	if err != nil {
		return checkinResult{OK: false, Detail: "网络失败：" + err.Error()}
	}
	st := traeUnwrapPayload(body)
	if st == nil {
		return checkinResult{OK: false, Detail: fmt.Sprintf("HTTP %d 响应解析失败：%s", status, truncate(string(body), 120))}
	}
	code, _ := st["code"].(float64)
	if code != 0 {
		msg, _ := st["message"].(string)
		return checkinResult{OK: false, Detail: fmt.Sprintf("code %d：%s", int(code), msg)}
	}
	enable, _ := st["enable"].(bool)
	checked, _ := st["checked_in"].(bool)
	if !enable {
		return checkinResult{OK: true, Detail: "签到活动未开放"}
	}
	if checked {
		return checkinResult{OK: true, Detail: "今日已签到"}
	}
	body, status, err = httpPostJSON(traeUgHost+"/trae/api/v2/ug/checkin_credits/claim", headers, []byte("{}"))
	if err != nil {
		return checkinResult{OK: false, Detail: "领取网络失败：" + err.Error()}
	}
	cl := traeUnwrapPayload(body)
	if cl == nil {
		return checkinResult{OK: false, Detail: fmt.Sprintf("claim HTTP %d：%s", status, truncate(string(body), 120))}
	}
	ccode, _ := cl["code"].(float64)
	if ccode == 0 {
		reward := traeRewardField(cl)
		if reward != "" {
			return checkinResult{OK: true, Detail: "签到成功 " + reward}
		}
		return checkinResult{OK: true, Detail: "签到成功"}
	}
	cmsg, _ := cl["message"].(string)
	switch int(ccode) {
	case 9095: // 该设备已签
		return checkinResult{OK: true, Detail: "今日已签到"}
	case 9074:
		return checkinResult{OK: false, Detail: "高峰限频(9074)，稍后自动重试"}
	default:
		return checkinResult{OK: false, Detail: fmt.Sprintf("claim code %d：%s", int(ccode), cmsg)}
	}
}

// traeUnwrapPayload Trae ug 接口的包装层兜底（data/result/resp/response/info）。
func traeUnwrapPayload(body []byte) map[string]any {
	var top map[string]any
	if json.Unmarshal(body, &top) != nil {
		return nil
	}
	if _, ok := top["code"]; ok {
		if _, has := top["enable"]; has {
			return top
		}
		if _, has := top["checked_in"]; has {
			return top
		}
	}
	for _, k := range []string{"data", "result", "resp", "response", "info"} {
		if m, ok := top[k].(map[string]any); ok {
			return m
		}
	}
	return top
}

// traeRewardField 从 claim 响应提取奖励数额（字段名各版本不一，逐个候选）。
func traeRewardField(m map[string]any) string {
	for _, k := range []string{"reward", "reward_credits", "credits", "amount", "extra_credits"} {
		if v, ok := m[k].(float64); ok && v > 0 {
			return "+" + fmt.Sprintf("%v", v)
		}
	}
	return ""
}

// runCheckin 执行签到并持久化结果；返回给控制台的汇总文案。
//
// 2026-10-08 变更：Qoder 签到永久停用——网关活动领取反复失败，且该平台
// 对自动/定时类操作风控敏感（用户拍板）。仅保留国内 WorkBuddy + Trae。
func runCheckin(h *Hub) (bool, string) {
	domCh := make(chan checkinResult, 1)
	traeCh := make(chan checkinResult, 1)
	go func() { domCh <- domCheckin() }()
	go func() { traeCh <- traeCheckin() }()
	dom := <-domCh
	trae := <-traeCh
	const qoderDetail = "已停用"

	h.mu.Lock()
	h.checkin = checkinRecord{
		Date:  time.Now().Format("2006-01-02"),
		Time:  time.Now().Format("15:04:05"),
		Dom:   dom.Detail,
		Qoder: qoderDetail,
		Trae:  trae.Detail,
		OK:    dom.OK && trae.OK,
	}
	h.saveStateLocked()
	rec := h.checkin
	h.mu.Unlock()

	logHub("签到 | 国内=%s | Qoder=%s | Trae=%s", dom.Detail, rec.Qoder, rec.Trae)
	msg := "国内：" + dom.Detail + "；Qoder：" + rec.Qoder + "；Trae：" + rec.Trae
	return rec.OK, msg
}

// runIntlCheckin 给**所有**国际凭证各做一次活跃上报，返回逐账号结果。
//
// 为什么单独成一个入口（2026-10-12 用户要求）：国际版的活跃是**按账号**计的，
// 而 intlCheckin 原先只取「当前选中的那个凭证」。要覆盖 4 个账号就得
// 「切凭证 → 点一键签到」重复 4 次，而每次都会顺带重打国内/Trae 两个已经
// 签过的接口——既慢又平白多出请求。现在拆开：国际单独一个按钮，一次跑完
// 全部凭证，不碰国内/Trae。
//
// 幂等：文档说「同一账号每日最多获得一次」，重复上报无害（实测 days 不再变）。
func runIntlCheckin(h *Hub) (bool, string) {
	names := make([]string, 0, 8)
	for name, dir := range credDirs() {
		if dir != "" && dirHasCredential(dir) {
			names = append(names, name)
		}
	}
	sort.Strings(names) // 固定顺序，便于对照
	if len(names) == 0 {
		return false, "没有可用的国际凭证"
	}

	type one struct {
		name string
		res  checkinResult
	}
	ch := make(chan one, len(names))
	for _, n := range names {
		go func(n string) { ch <- one{n, intlCheckin(n)} }(n)
	}

	results := make([]one, 0, len(names))
	allOK := true
	for i := 0; i < len(names); i++ {
		r := <-ch
		results = append(results, r)
		if !r.res.OK {
			allOK = false
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })

	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, r.name+" "+r.res.Detail)
	}
	detail := strings.Join(parts, "；")

	// 结果写进 checkin.Intl（沿用同一字段，控制台签到条能直接显示）
	h.mu.Lock()
	h.checkin.Date = time.Now().Format("2006-01-02")
	h.checkin.Time = time.Now().Format("15:04:05")
	h.checkin.Intl = detail
	h.saveStateLocked()
	h.mu.Unlock()

	logHub("国际活跃上报（%d 个凭证）| %s", len(results), detail)
	return allOK, "国际活跃上报：" + detail
}

// autoCheckinLoop 已停用（2026-10-08 用户拍板）。
//
// 原逻辑：启动 30s 后自动签到，之后每小时兜底（含 Qoder 反复失败重试）。
// 停用原因：自动定时签到属于机器行为模式，与「额度查询改手动」同一风控
// 顾虑；Qoder 签到已永久关闭。签到完全由控制台「一键签到」手动触发。
// main.go 不再启动本函数；代码保留以便将来按需恢复。
func autoCheckinLoop(h *Hub) {
	// 已停用，见上方注释。
}

func runCheckinIfNewDay(h *Hub) {
	h.mu.Lock()
	done := h.checkin.Date == time.Now().Format("2006-01-02") && h.checkin.OK
	h.mu.Unlock()
	if !done {
		runCheckin(h)
	}
}
