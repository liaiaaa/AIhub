package main

// 国内 WorkBuddy 多凭证看板（控制台「线路与凭证」页的卡片）。
//
// 2026-17 用户要求：国内版那个 `<select>` + 「换凭证并重启」的切换太丑，
// 改成国际版那样的凭证卡片列表（谁可用、余额多少、限到几点一眼看得见）。
//
// 复用 intlCredStatus 这个结构而不是另建一个：前端 credBadgeText/credWhy/
// credTitle/卡片渲染全都按它写字段，两边形状一致才能共用一套 UI 与文案口径，
// 否则国内会慢慢长出一套「只有余额、没有可用性」的简化版看板。
//
// 与 intl 的唯一差别是**可用性怎么判定**：
//   - intl 有 intlProbeInfer（打 workbuddy.ai 的 chat 端点）；
//   - dom 此前没有任何探活，只有 billing 余额接口。而 billing 查得到 ≠ 推理通道通
//     （2026-10-12 在 intl 上实测过：额度 200、userinfo 200，chat 却 403 11140）。
//     所以这里补一个 domProbeInfer，形状照 intl 那套，不拿余额充当可用性。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// postJSONWithClient 与 httpPostJSON 同语义，但用调用方给的 client。
// 必须能换 client：国内线路是 DirectOnly（走 directClient 忽略环境代理），
// 用裸 client 的话探测/查额度走的网络路径和真实推理不一样，会出现
// 「线路能用、看板说凭证不可用」这类假故障（2026-10-15 在 intl 上踩过）。
func postJSONWithClient(client *http.Client, url string, headers map[string]string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

// domProbeModel 探活用的模型：国内表里最便宜的（0.05x，见 model_ratios.go），
// 且必须在 dom 的 Models 清单里，否则探活打到不存在的模型会被上游拒，
// 把「凭证是好的」误判成不可用。
const domProbeModel = "hy3-preview-agent"

// domProbeBody 与 intlProbeBody 同形状：max_tokens=1 + 流式，读到首帧即判。
func domProbeBody() []byte {
	body, _ := json.Marshal(map[string]any{
		"model": domProbeModel,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
		"stream":     true,
	})
	return body
}

// domProbeInfer 真打一次国内推理通道。
//
// 三条必须与推理路径一致的经验（都是踩出来的）：
//  1. 头要用 p.domProbeHeaders —— 国内后端同样可能加通路校验，两条构造路径
//     给不同头就会出现「推理能用、看板全红」；
//  2. 复用线路的 http.Client（DirectOnly 线路走 directClient，忽略环境代理），
//     别自己 new 一个裸 client，否则探测和真实推理走的不是同一条网络路径；
//  3. 失败要把**原因**带出来，不能全塌成「探活失败」。
func domProbeInfer(cm *CredentialManager, p *Proxy) (probeResult, string) {
	headers, err := cm.Headers()
	if err != nil {
		return probeUnknown, "取认证头失败：" + err.Error()
	}
	headers = p.domProbeHeaders(headers)
	req, err := http.NewRequest("POST", p.Backend+"/v2/chat/completions", bytes.NewReader(domProbeBody()))
	if err != nil {
		return probeUnknown, "构造请求失败：" + err.Error()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	_, _, client := p.runtime()
	if client == nil {
		client = newBackendClient() // 与线路同源：认 GNOME 系统代理 + 死代理自动回落直连
		client.Timeout = 20 * time.Second
	}
	return intlProbeDo(client, req, "") // 响应归类与 intl 完全同族（同后端体系，错误码一致）
}

// domProbeHeaders 见 intlProbeHeaders 的注释：探活不走 doUpstream，得自己补一套。
func (p *Proxy) domProbeHeaders(cmHeaders map[string]string) map[string]string {
	cmHeaders["User-Agent"] = p.UserAgent
	cmHeaders["Content-Type"] = "application/json"
	return p.withConversationHeaders(cmHeaders)
}

// domCreditsRaw 用指定凭证查国内余额（不依赖线路是否在跑）。
func domCreditsRaw(cm *CredentialManager, p *Proxy) (remain, total float64, ok bool) {
	headers, err := cm.Headers()
	if err != nil {
		return -1, -1, false
	}
	// billing 域只认 WorkBuddy/<版本> 形态的 UA，送错 UA 回业务码 10085（见 credits.go 顶部证据）。
	headers["User-Agent"] = "WorkBuddy/5.5.4"
	headers["X-Product"] = "SaaS"
	headers["X-IDE-Type"] = "WorkBuddy"
	_, _, client := p.runtime()
	if client == nil {
		client = newBackendClient()
		client.Timeout = 20 * time.Second
	}
	body, status, err := postJSONWithClient(client, p.Backend+"/billing/meter/get-user-resource-summary", headers, []byte("{}"))
	if err != nil || status != http.StatusOK {
		return -1, -1, false
	}
	r, t, ok := wbSummaryParse(body)
	if !ok {
		return -1, -1, false
	}
	return r, t, true
}

// domCredStatusOne 单个国内凭证的看板条目。
//
// 判定顺序与 intlCredStatusOne 保持一致：无凭据 → 本地台账 → 真探活。
// 台账放在探活前面是因为它更快且不消耗额度；但台账没记录**不等于**能用，
// 所以最后必须真打一次。
func domCredStatusOne(name, dir string, selected bool) intlCredStatus {
	st := intlCredStatus{Name: name, Remain: -1, Selected: selected, State: "未知"}

	p := NewDomProxyDir(dir)
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
	if remain, total, ok := domCreditsFn(cm, p); ok {
		st.Remain, st.Total = remain, total
	}

	if e, limited := rateLimitFor(st.UID, ""); limited {
		st.OK = false
		st.Reason = e.Reason
		st.UpstreamReset = e.HasUpstreamReset()
		if e.Kind == rlKindAccount {
			st.State = "风控"
			if st.UpstreamReset {
				st.Until = rateLimitShortUntil(e.Until)
				st.RemainTx = rateLimitRemain(e.Until)
			}
		} else {
			st.State = "限流"
			st.Until = rateLimitShortUntil(e.Until)
			st.RemainTx = rateLimitRemain(e.Until)
		}
		st.ModelLimits = modelLimitsFor(st.UID)
		return st
	}

	probe, probeDetail := domProbeInferFn(cm, p)
	st.ModelLimits = modelLimitsFor(st.UID) // 账号通道通、个别模型被限：单独列出来
	switch probe {
	case probeOK:
		st.OK = true
		st.State = "可用"
	case probeRisk:
		st.State = "风控"
		st.Reason = probeDetail
	case probeRateLimited:
		st.State = "限流"
		st.Reason = probeDetail
	default:
		st.State = "探活失败"
		st.Reason = probeDetail
	}
	return st
}

// domProbeInferFn 做成变量只为测试可注入：看板逻辑不能靠反复打上游来验证。
var domProbeInferFn = domProbeInfer

// domCreditsFn 同上，供测试替换。
var domCreditsFn = domCreditsRaw

// domCredsStatus 并发列出全部国内凭证的状态。
func domCredsStatus(h *Hub, force bool) []intlCredStatus {
	h.mu.Lock()
	selected := h.domCred
	h.mu.Unlock()

	dirs := domCredDirs()
	names := make([]string, 0, len(dirs))
	for name, dir := range dirs {
		if dir != "" && dirHasCredential(dir) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]intlCredStatus, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			out[i] = domCredStatusOne(name, dirs[name], name == selected)
		}(i, name)
	}
	wg.Wait()
	return out
}
