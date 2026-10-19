package main

// 上游模型目录：GET {Backend}/v3/config
//
// 2026-10-16 挖到的（找 intl 通路校验时顺手发现）。它按账号下发**权威的**模型清单，
// 每个对话模型都带：
//
//	credits          "x0.79 credits"     —— 倍率
//	contextWindow    {defaultLength: 300000, supportedLengths: [300000, 1000000]}
//	maxInputTokens   数字
//	descriptionZh    中文说明
//
// 这三样此前全靠手工维护：倍率抄 倍率/workbuddy国际.md、上下文抄 staticModelMeta
// （model_meta.go 里 33 个写死的条目）。用户 2026-10-15 抱怨「模型没有倍率和上下文」、
// 「国际线路里没有新模型」，根因就是手工表跟不上上游上新。改读这个接口后，
// 上游加模型这边自动跟。
//
// 两个坑（都是实测撞出来的）：
//  1. UA 必须是 WorkBuddy/<版本>（国内是 CodeBuddy/<版本>）。推理用的
//     codebuddy-guoji/1.0 打这个接口会被 400 拒：
//     {"code":12403,"msg":"check ua, get coding copilot version error"}
//     注意大小写敏感 —— workbuddy-desktop/5.7.6 一样被拒。
//  2. 目录里列了 ≠ 能用。hy3-preview / gpt-6.1-sol 在别的接口里回过
//     400「service info not found」，gpt-6-sol 更阴：HTTP 200 但
//     finish_reason=error、usage 全 0（假 200）。所以本文件**只供元数据**，
//     绝不自动往可路由清单里塞模型 —— 录入仍走实测 + 控制台手动添加。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// wbCatalogEntry 目录里一个模型。
type wbCatalogEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`        // 官方显示名（可能缺，缺则用 id）
	Desc      string `json:"desc,omitempty"`        // descriptionZh
	Ratio     string `json:"ratio,omitempty"`       // "0.79x" / "免费"；空 = 上游没给
	Context   int    `json:"context,omitempty"`     // 能配的最大上下文（supportedLengths 取大）
	Default   int    `json:"default,omitempty"`     // 上游默认档位
	MaxInput  int    `json:"max_input,omitempty"`   // 输入上限
	Chat      bool   `json:"chat"`                  // 是否对话模型（图像/视频模型为 false）
	IsDefault bool   `json:"is_default,omitempty"` // 上游标的主模型
}

// wbCatalog 一次抓取的快照。
type wbCatalog struct {
	Line      string           `json:"line"`
	FetchedAt time.Time        `json:"fetched_at"`
	Entries   []wbCatalogEntry `json:"entries"`
}

func (c *wbCatalog) byID() map[string]wbCatalogEntry {
	m := make(map[string]wbCatalogEntry, len(c.Entries))
	for _, e := range c.Entries {
		m[e.ID] = e
	}
	return m
}

const (
	wbCatalogTTL = 6 * time.Hour // 上游上新是天级的，没必要更勤
)

var (
	wbCatalogMu    sync.RWMutex
	wbCatalogCache = map[string]*wbCatalog{} // line -> 最近一次成功快照
)

// wbCatalogApplies 这条线路是否该用上游 /v3/config 目录。
//
// 只有 intl 用。dom（copilot.tencent.com）实测**不适用**，两个硬伤：
//  1. 里面根本没有 credits 字段 → 一个倍率都拿不到；
//  2. 列的是 IDE 补全那一套老 id（glm-4.6 / deepseek-v3-0324 / codewise-* 等 37 个），
//     hub-go 实际在路由的 glm-5.3、glm-5.2 反而**不在**里面。拿它当上游目录，
//     等于用一份更旧、还缺倍率的表去覆盖现有的手工表，只会更差。
// 所以 dom 继续走 倍率/workbuddy国内.md 那套静态表；这里留口子是为了让
// 「dom 不适用」这件事有个能改的地方，而不是散在各分支里。
func wbCatalogApplies(line string) bool { return line == "intl" }

// wbCatalogPath 快照落盘位置。线路没启动也能显示倍率/上下文，靠的就是这份缓存。
func wbCatalogPath(line string) string { return dataPath("wb_catalog_" + line + ".json") }

// wbCatalogUA 目录接口认的 UA。
//
// 起初按产品名猜：国内后端是 copilot.tencent.com、客户端自称 CodeBuddy，于是给
// 它发 CodeBuddy/<版本>。实测**猜错**，同一凭据下
//
//	UA=CodeBuddy/5.7.6 → data 只剩 enterpriseId/productFeatures，根本没有 models
//	UA=WorkBuddy/5.7.6 → data.models 37 个
//
// 即这个接口对两条线路都只认 WorkBuddy/<版本>。保留这个函数而不是写死一处，
// 是因为它已经证明「UA 决定响应形状」；上游真分化时这里是一个明确的改动点。
func wbCatalogUA(line string) string {
	return "WorkBuddy/" + wbCatalogVersion
}

const wbCatalogVersion = "5.7.6"

// 上游 credits 文案形态："x0.79 credits" / "x0.00" / "0.79"。
var wbCreditsRe = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)`)

// parseWBCredits 把 credits 文案解析成倍率标签。拿不到数字就返回空串——
// 宁可少标注，也不要瞎猜（2026-10-13 用关键词启发式把付费模型标成「免费」过，
// 用户当场指出，见 model_probe.go 顶部注释）。
func parseWBCredits(s string) string {
	if s == "" {
		return ""
	}
	m := wbCreditsRe.FindString(s)
	if m == "" {
		return ""
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return ""
	}
	if f == 0 {
		return "免费"
	}
	return strconv.FormatFloat(f, 'f', -1, 64) + "x"
}

// firstNumber 从 maxInputTokens 之类字段里取整数：可能是 number，也可能是字符串。
func firstNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i)
		}
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return int(f)
		}
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// parseWBConfigBody 把 /v3/config 响应解析成目录快照。
// 单独成函数是为了能在离线单测里喂真实样本，不依赖网络。
func parseWBConfigBody(body []byte, line string) (*wbCatalog, error) {
	var top struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []map[string]any `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("解析 /v3/config 失败：%v", err)
	}
	if top.Code != 0 {
		return nil, fmt.Errorf("/v3/config 业务码 %d：%s", top.Code, top.Msg)
	}
	cat := &wbCatalog{Line: line, FetchedAt: time.Now()}
	for _, m := range top.Data.Models {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		e := wbCatalogEntry{ID: id, Chat: true}
		if n, _ := m["name"].(string); n != "" {
			e.Name = n
		}
		if z, _ := m["descriptionZh"].(string); z != "" {
			e.Desc = z
		}
		if c, _ := m["credits"].(string); c != "" {
			e.Ratio = parseWBCredits(c)
		}
		// contextWindow 有两种形态：{defaultLength,supportedLengths[]} 或直接数字
		switch cw := m["contextWindow"].(type) {
		case map[string]any:
			e.Default = firstNumber(cw["defaultLength"])
			if sl, ok := cw["supportedLengths"].([]any); ok {
				for _, v := range sl {
					e.Context = maxInt(e.Context, firstNumber(v))
				}
			}
			if e.Context == 0 {
				e.Context = e.Default
			}
		default:
			e.Context = firstNumber(m["contextWindow"])
			e.Default = e.Context
		}
		e.MaxInput = firstNumber(m["maxInputTokens"])
		if e.Context == 0 {
			e.Context = e.MaxInput
		}
		if d, _ := m["isDefault"].(bool); d {
			e.IsDefault = true
		}
		// 图像/视频模型没有输入上限，只有 tags —— 混进对话目录会让客户端选了必挂。
		if e.Context == 0 && e.MaxInput == 0 {
			e.Chat = false
		}
		cat.Entries = append(cat.Entries, e)
	}
	if len(cat.Entries) == 0 {
		return nil, fmt.Errorf("/v3/config 里没解析出任何模型（上游改结构了？）")
	}
	return cat, nil
}

// fetchWBCatalog 打一次目录接口。headers 来自 CredentialManager，
// UA 换成接口认的那个（见 wbCatalogUA）。
func fetchWBCatalog(client *http.Client, backend, line string, headers map[string]string) (*wbCatalog, error) {
	req, err := http.NewRequest("GET", backend+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", wbCatalogUA(line))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer io.Copy(io.Discard, resp.Body)
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncate(string(body), 120))
	}
	return parseWBConfigBody(body, line)
}

// wbCatalogLoadDisk 读落盘快照（线路没启动时的数据来源）。
func wbCatalogLoadDisk(line string) *wbCatalog {
	b, err := os.ReadFile(wbCatalogPath(line))
	if err != nil {
		return nil
	}
	var c wbCatalog
	if json.Unmarshal(b, &c) != nil || len(c.Entries) == 0 {
		return nil
	}
	return &c
}

// wbCatalogGet 取某条线路的目录：内存 → 磁盘。都没有则返回 nil。
// 调用方拿到 nil 必须能正常降级（回到手工静态表）。
func wbCatalogGet(line string) *wbCatalog {
	if !wbCatalogApplies(line) {
		return nil
	}
	wbCatalogMu.RLock()
	c := wbCatalogCache[line]
	wbCatalogMu.RUnlock()
	if c != nil && time.Since(c.FetchedAt) < wbCatalogTTL {
		return c
	}
	disk := wbCatalogLoadDisk(line)
	if disk != nil && time.Since(disk.FetchedAt) < wbCatalogTTL {
		wbCatalogMu.Lock()
		wbCatalogCache[line] = disk
		wbCatalogMu.Unlock()
		return disk
	}
	return c // 过期快照仍比没有强，交由调用方决定是否刷新
}

// wbCatalogPut 存快照到内存和磁盘。
func wbCatalogPut(c *wbCatalog) {
	wbCatalogMu.Lock()
	wbCatalogCache[c.Line] = c
	wbCatalogMu.Unlock()
	if b, err := json.MarshalIndent(c, "", " "); err == nil {
		_ = os.WriteFile(wbCatalogPath(c.Line), b, 0644)
	}
}

// wbCatalogLookup 查一个模型的目录条目。
func wbCatalogLookup(line, model string) (wbCatalogEntry, bool) {
	c := wbCatalogGet(line)
	if c == nil {
		return wbCatalogEntry{}, false
	}
	e, ok := c.byID()[model]
	return e, ok
}

// wbCatalogRatioLabel 上游给的倍率文案；没有则空串（调用方回退静态表）。
func wbCatalogRatioLabel(line, model string) string {
	if e, ok := wbCatalogLookup(line, model); ok {
		return e.Ratio
	}
	return ""
}

// wbCatalogContext 上游给的上下文（取能配到的最大值）。
func wbCatalogContext(line, model string) int {
	if e, ok := wbCatalogLookup(line, model); ok {
		return e.Context
	}
	return 0
}

// refreshWBCatalog 抓一次上游目录。force=false 时尊重 TTL，避免每次看板刷新
// 都去打上游接口白耗配额；抓不动（线路没起、没凭据、网络不通）返回 nil，
// 调用方自己回落到快照或手工静态表 —— 目录抓不到绝不能把线路带崩。
func (h *Hub) refreshWBCatalog(line string, p *Proxy, force bool) *wbCatalog {
	if !wbCatalogApplies(line) {
		return nil
	}
	path := p.AuthFilePath()
	if path == "" || !fileExists(path) {
		return nil
	}
	if c := wbCatalogGet(line); c != nil && !force && time.Since(c.FetchedAt) < wbCatalogTTL {
		return c // TTL 内不重复打上游
	}
	cm := NewCredentialManager(path, p.Backend, p.DefaultDomain, p.UserAgent)
	headers, err := cm.Headers()
	if err != nil {
		logHub("%s 目录刷新失败：取认证头失败：%v", line, err)
		return nil
	}
	client := newBackendClient()
	client.Timeout = 20 * time.Second
	cat, err := fetchWBCatalog(client, p.Backend, line, headers)
	if err != nil {
		logHub("%s 目录刷新失败（沿用旧快照/手工表）：%v", line, err)
		return nil
	}
	wbCatalogPut(cat)
	logHub("%s 上游目录已刷新：%d 个条目（%d 个对话模型）", line, len(cat.Entries), wbCatalogChatCount(cat))
	return cat
}

func wbCatalogChatCount(c *wbCatalog) int {
	n := 0
	if c != nil {
		for _, e := range c.Entries {
			if e.Chat {
				n++
			}
		}
	}
	return n
}

// wbCatalogRatioValue 上游倍率的数值形态，供 modelRatio 这类要 float 的调用方用。
func wbCatalogRatioValue(line, model string) (float64, bool) {
	e, ok := wbCatalogLookup(line, model)
	if !ok || e.Ratio == "" {
		return 0, false
	}
	if e.Ratio == "免费" {
		return 0, true
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(e.Ratio, "x"), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// forceWBDirectory 控制台「检查新模型」是用户主动点的，该重抓；
// 后台自动刷新走 TTL。
const forceWBDirectory = true

// warmWBCatalogs 开机后台把两条 WorkBuddy 线路的上游目录各抓一次。
//
// 为什么要有这一步：/v1/models 的显示名（倍率 + 上下文）现在以上游目录优先，
// 而目录原本只有用户点控制台「检查新模型」时才抓。开机后没人点，就只剩磁盘上的
// 旧快照 —— 首次运行连快照都没有，模型名会退化成裸 slug（正是用户 2026-10-15 报的
// 「选聚合网关模型显示英文 slug、没有倍率」）。抓不到就安静回落，不打扰启动。
func (h *Hub) warmWBCatalogs() {
	h.mu.Lock()
	intlCred, domCred := h.intlCred, h.domCred
	h.mu.Unlock()

	if dir := credDirs()[intlCred]; dir != "" {
		h.refreshWBCatalog("intl", NewIntlProxy(dir), true)
	}
	_ = domCred // dom 不适用上游目录（见 wbCatalogApplies），只热 intl
}

// wbCatalogFallbackNote 用不上上游目录时，控制台该显示哪句说明。
// 分线路写，是因为「dom 不适用」和「intl 这次没抓到」是两件不同的事，
// 混成一句会让人以为 dom 也只是偶发失败、重试就好。
func wbCatalogFallbackNote(line string) string {
	if !wbCatalogApplies(line) {
		return "倍率来自公开资料+实测（model_ratios.go）；该线路的上游目录不含倍率且模型更旧，不适用（见 wbCatalogApplies）"
	}
	return "倍率来自公开资料+实测（model_ratios.go）；这次没抓到上游目录，已回落手工表"
}
