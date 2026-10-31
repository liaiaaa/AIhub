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
	"strconv"
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
	headers["User-Agent"] = officialDomBillingUA()
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
func domCredStatusOne(name, dir string, selected bool, force bool) intlCredStatus {
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
	st.Expiries = wbCreditExpiries(cm, p, force)

	if e, limited := rateLimitFor(st.UID, ""); limited {
		st.OK = false
		st.Reason = e.Reason
		st.UpstreamReset = e.HasUpstreamReset()
		switch e.Kind {
		case rlKindAccount:
			st.State = "风控"
			// 风控（11140）上游从不给恢复时刻：只报"被拦"，不编时间。
			if st.UpstreamReset {
				st.Until = rateLimitShortUntil(e.Until)
				st.RemainTx = rateLimitRemain(e.Until)
			}
		case rlKindChannel:
			// 11128 闸门是本地暂停，上游没给任何时刻；Until 就是本地下次放行的时刻。
			st.State = "渠道拦截"
			st.Until = rateLimitShortUntil(e.Until)
			st.RemainTx = rateLimitRemain(e.Until)
		default:
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
var (
	domCredsMu    sync.Mutex
	domCredsCache []intlCredStatus
	domCredsHave  bool
)

// domCredsCacheReset 清空凭证卡片缓存。测试隔离用；生产里缓存只被
// force=true（手动「刷新额度」/ 启动预热）覆盖。
func domCredsCacheReset() {
	domCredsMu.Lock()
	domCredsCache, domCredsHave = nil, false
	domCredsMu.Unlock()
}

// domCredsStatus 国内凭证卡片数据。
//
// 2026-10-20 用户拍板：**启动时拉一次就够，之后只吃缓存**，手动点「刷新额度」
// 才真正重打上游。此前每次进线路页都对每个凭证发一串请求（Summary + 余额 +
// 推理探活 + 积分到期 3 连发），凭证多的时候一开页面就是十几个上游调用。
// 缓存没有 TTL——过期只由 force 驱动。唯一每读都要刷新的字段是 Selected
// （切凭证不该等用户手动刷新才高亮），读出时按当前 domCred 现算。
func domCredsStatus(h *Hub, force bool) []intlCredStatus {
	h.mu.Lock()
	selected := h.domCred
	h.mu.Unlock()

	domCredsMu.Lock()
	cached, have := domCredsCache, domCredsHave
	domCredsMu.Unlock()
	if have && !force {
		out := make([]intlCredStatus, len(cached))
		for i, st := range cached {
			st.Selected = st.Name == selected
			out[i] = st
		}
		return out
	}

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
			defer recoverGoroutine("cred_dom.go:258")
			defer wg.Done()
			out[i] = domCredStatusOne(name, dirs[name], name == selected, force)
		}(i, name)
	}
	wg.Wait()

	domCredsMu.Lock()
	domCredsCache, domCredsHave = out, true
	domCredsMu.Unlock()
	return out
}

// ---------- 积分到期显示（2026-10-20 用户要求） ----------
//
// 背景：用户的另一个会话在跑 WorkBuddy 成长中心领积分，领到的都是**限时积分**
// （礼包 1500 到 10-31、补偿 500 到 10-22）。哪个凭证的积分快到期就该先用哪个，
// 但控制台凭证卡片上只有总余额，看不出「里面有多少是几号到期的」。
//
// 数据源（两个都是 POST {} 空体，billing 域，UA 必须 WorkBuddy/5.5.4）：
//   POST /billing/meter/check-gift-claimed  → data{claimed,active,credit_num,end_time}
//   POST /billing/meter/compensation-status → data{claimed,active,credit_num,end_time}
// 剩余量与资源包汇总（get-user-resource-summary 的 Packages）按金额对账：
// CycleTotalCapacity == credit_num 视为同一个包，取它的 CycleRemainCapacity；
// 对不上就不显示剩余、只给到期日（如某号礼包授予 1500 但包总量是 1000）。
// 剩余为 0 的包整个隐藏——用完了的积分不存在「到期」，显示反而吓人。

// credExpiry 凭证卡片上的单条到期提醒。
type credExpiry struct {
	Label  string `json:"label"`            // 礼包 / 补偿
	Credit string `json:"credit"`           // 授予积分数
	End    string `json:"end"`              // MM-DD
	Remain string `json:"remain,omitempty"` // 该包剩余（能对上资源包时才有）
}

var (
	wbExpiryMu    sync.Mutex
	wbExpiryCache = map[string]wbExpiryEntry{}
)

type wbExpiryEntry struct {
	At    time.Time
	Items []credExpiry
}

// wbExpiryTTL 到期信息的缓存时长。数据一天变不了几次，卡片刷新又较频繁，
// 不缓存的话每次刷新都打 3 个上游接口 × 凭证数。
const wbExpiryTTL = 10 * time.Minute

// wbCreditExpiries 某个国内凭证的限时积分列表（按到期日升序）。
// force=true（手动「刷新额度」）绕过 TTL 直接重拉；平时只吃缓存，
// 配合 domCredsStatus 的进程级缓存，做到「启动拉一次，之后不再动」。
func wbCreditExpiries(cm *CredentialManager, p *Proxy, force bool) []credExpiry {
	key := p.AuthFilePath()
	now := time.Now()
	wbExpiryMu.Lock()
	if !force {
		if e, ok := wbExpiryCache[key]; ok && now.Sub(e.At) < wbExpiryTTL {
			wbExpiryMu.Unlock()
			return e.Items
		}
	}
	wbExpiryMu.Unlock()

	items := wbCreditExpiriesFetch(cm, p)

	wbExpiryMu.Lock()
	wbExpiryCache[key] = wbExpiryEntry{At: now, Items: items}
	wbExpiryMu.Unlock()
	return items
}

func wbCreditExpiriesFetch(cm *CredentialManager, p *Proxy) []credExpiry {
	headers, err := cm.Headers()
	if err != nil {
		return nil
	}
	headers["User-Agent"] = officialDomBillingUA()
	headers["X-Product"] = "SaaS"
	headers["X-IDE-Type"] = "WorkBuddy"
	_, _, client := p.runtime()
	if client == nil {
		client = newBackendClient()
		client.Timeout = 20 * time.Second
	}
	post := func(path string) map[string]any {
		body, status, err := postJSONWithClient(client, p.Backend+path, headers, []byte("{}"))
		if err != nil || status != http.StatusOK {
			return nil
		}
		var env struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(body, &env) != nil || env.Code != 0 {
			return nil
		}
		return env.Data
	}

	var out []credExpiry
	add := func(label string, d map[string]any) {
		if d == nil {
			return
		}
		claimed, _ := d["claimed"].(bool)
		active, _ := d["active"].(bool)
		if !claimed || !active {
			return
		}
		end := wbShortDate(d["end_time"])
		if end == "" {
			return
		}
		out = append(out, credExpiry{
			Label:  label,
			Credit: flexCreditString(d["credit_num"]),
			End:    end,
		})
	}
	add("礼包", post("/billing/meter/check-gift-claimed"))
	add("补偿", post("/billing/meter/compensation-status"))
	if len(out) == 0 {
		return nil
	}

	// 与资源包对剩余量（见块顶注释的对账规则）。
	if body, status, err := postJSONWithClient(client, p.Backend+"/billing/meter/get-user-resource-summary", headers, []byte("{}")); err == nil && status == http.StatusOK {
		var sum struct {
			Data struct {
				Packages []struct {
					CycleRemainCapacity flexNum `json:"cycleRemainCapacity"`
					CycleTotalCapacity  flexNum `json:"cycleTotalCapacity"`
				} `json:"packages"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &sum) == nil {
			kept := out[:0]
			for _, e := range out {
				grant, errP := strconv.ParseFloat(e.Credit, 64)
				if errP != nil {
					kept = append(kept, e)
					continue
				}
				matched := false
				for _, pkg := range sum.Data.Packages {
					if float64(pkg.CycleTotalCapacity) != grant {
						continue
					}
					matched = true
					remain := float64(pkg.CycleRemainCapacity)
					if remain <= 0 {
						break // 用完了：整条隐藏
					}
					e.Remain = strconv.FormatInt(int64(remain+0.5), 10)
					kept = append(kept, e)
					break
				}
				if !matched {
					kept = append(kept, e) // 对不上包：只给到期日
				}
			}
			out = kept
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(a, b int) bool { return out[a].End < out[b].End })
	return out
}

// wbShortDate "2026-10-31 23:59:59" → "10-31"；解析失败返回空。
func wbShortDate(v any) string {
	s, _ := v.(string)
	if len(s) < 10 {
		return ""
	}
	if s[4] != '-' || s[7] != '-' {
		return ""
	}
	return s[5:10]
}

// flexCreditString credit_num 在 map[string]any 里是 float64（或字符串），转展示串。
func flexCreditString(v any) string {
	switch n := v.(type) {
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	}
	return ""
}
