package main

// 线路适配器注册表（A2 第二步，2026-11-02）。
//
// 起因：用户问「我要是再添加一个工具，还得重新破解实现反代吗？」
// 此前确实如此 —— 新增一条线路要动 6 个地方：端口常量、lines.go 元数据、
// hub_api.go 的 lineToggles、main.go 的 /api/<key>/start 路由、hub.go 的状态详情、
// proxy_agg.go 的 lineRunning 与转发 switch，外加一个上千行的 handleChat。
//
// 现在把「这条线路能做什么」收进一个接口 + 一张构造表：
//
//	新增一条线路 =
//	  1) lines.go 的 lineSpecs 加一行元数据（名称/显示名/端口/日志/是否聚合/风控）；
//	  2) 写一个文件实现 lineAdapter（认证 + 请求体转换 + 流式解析，这部分无解，
//	     每条厂商协议都不一样 —— magpie 的 internal/provider/ 也有 241 个文件）；
//	  3) 在 lineAdapters 里登记一行构造器。
//	路由、启停、状态、聚合转发、模型目录、用量归类全部按注册表自动生效。
//
// 安全边界（这次重构刻意不碰）：
//   - 不动每个 handleChat 的内部实现（4 份 streamToClient 各管一种协议，合并需要 IR 层，
//     是另一个项目级工作量，收益低风险高，已明确不做）；
//   - 不改任何请求/响应字节：适配器只是把已有的方法挂到统一入口上。
// 回归保障：`.tmp/ab_probe.py` 对 18 个端点的响应做逐字节比对（见 lines.go 注释）。

import "net/http"

// lineAdapter 一条线路的运行时能力。所有方法都必须能安全并发调用。
type lineAdapter interface {
	// Key 与 lineSpecs 的 Key 一致（/api/<Key>/start 的路径段）。
	Key() string
	// Running 该线路当前是否在监听。
	Running() bool
	// Start 启动线路；返回 (是否成功, 给用户看的中文提示)。
	Start() (bool, string)
	// Stop 停止线路；返回 (是否已释放, 提示)。
	Stop() (bool, string)
	// Chat 处理**已经是该线路原生 chat/completions 形态**的请求。
	// 聚合口负责把 /v1/responses 等转成 chat 再调它。
	Chat(w http.ResponseWriter, r *http.Request)
	// NativeResponses 线路原生支持的 /v1/responses handler；nil = 只支持 chat。
	// 目前只有 dom/intl（WorkBuddy）有原生 responses。
	NativeResponses() http.HandlerFunc
	// Models 线路对外宣称的模型清单；nil = 由上游目录/数据文件动态提供，不在这里枚举。
	Models() []string
}

// proxyLineAdapter dom/intl 共用 Proxy 类型，靠 key 区分。
type proxyLineAdapter struct {
	h   *Hub
	key string
}

func (a proxyLineAdapter) Key() string { return a.key }

func (a proxyLineAdapter) proxy() *Proxy { return a.h.proxyForLine(a.key) }

func (a proxyLineAdapter) Running() bool {
	p := a.proxy()
	return p != nil && p.Running()
}

func (a proxyLineAdapter) Start() (bool, string) {
	switch a.key {
	case "intl":
		a.h.mu.Lock()
		cred := a.h.intlCred
		a.h.mu.Unlock()
		return a.h.startIntl(cred)
	default:
		return a.h.startDom()
	}
}

func (a proxyLineAdapter) Stop() (bool, string) {
	if a.key == "intl" {
		return a.h.stopIntl()
	}
	return a.h.stopDom()
}

func (a proxyLineAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	if p := a.proxy(); p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "线路 " + a.key + " 未初始化", "type": "line_unavailable"},
	})
}

func (a proxyLineAdapter) NativeResponses() http.HandlerFunc {
	p := a.proxy()
	if p == nil {
		return nil
	}
	return p.handleResponses
}

func (a proxyLineAdapter) Models() []string { return nil }

// qoderAdapter Qoder：独立协议（只有 chat/completions）。
type qoderAdapter struct{ h *Hub }

func (a qoderAdapter) Key() string { return "qoder" }

func (a qoderAdapter) Running() bool {
	p := a.h.qoderForAgg()
	return p != nil && p.Running()
}

func (a qoderAdapter) Start() (bool, string) { return a.h.startQoder() }
func (a qoderAdapter) Stop() (bool, string)  { return a.h.stopQoder() }

func (a qoderAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	if p := a.h.qoderForAgg(); p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "Qoder 线路未初始化", "type": "line_unavailable"},
	})
}

func (a qoderAdapter) NativeResponses() http.HandlerFunc { return nil }

func (a qoderAdapter) Models() []string {
	p := a.h.qoderForAgg()
	if p == nil {
		return nil
	}
	entries := p.catalogSnapshot()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}

// traeAdapter Trae：独立协议，静态模型清单。
type traeAdapter struct{ h *Hub }

func (a traeAdapter) Key() string { return "trae" }

func (a traeAdapter) Running() bool {
	p := a.h.traeForAgg()
	return p != nil && p.Running()
}

func (a traeAdapter) Start() (bool, string) { return a.h.startTrae() }
func (a traeAdapter) Stop() (bool, string)  { return a.h.stopTrae() }

func (a traeAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	if p := a.h.traeForAgg(); p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "Trae 线路未初始化", "type": "line_unavailable"},
	})
}

func (a traeAdapter) NativeResponses() http.HandlerFunc { return nil }

func (a traeAdapter) Models() []string {
	out := make([]string, len(TraeModels))
	copy(out, TraeModels)
	return out
}

// teleagentAdapter TeleAgent（TeleAI）：OpenAI 兼容，但要 super agent 签名头。
type teleagentAdapter struct{ h *Hub }

func (a teleagentAdapter) Key() string { return "teleagent" }

func (a teleagentAdapter) Running() bool {
	p := a.h.teleagentForAgg()
	return p != nil && p.Running()
}

func (a teleagentAdapter) Start() (bool, string) { return a.h.startTeleagent() }
func (a teleagentAdapter) Stop() (bool, string)  { return a.h.stopTeleagent() }

func (a teleagentAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	if p := a.h.teleagentForAgg(); p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "TeleAgent 线路未初始化", "type": "line_unavailable"},
	})
}

func (a teleagentAdapter) NativeResponses() http.HandlerFunc { return nil }

func (a teleagentAdapter) Models() []string {
	out := make([]string, 0, len(teleagentModels))
	for _, m := range teleagentModels {
		if !m.Disabled {
			out = append(out, m.ID)
		}
	}
	return out
}

// zcodeAdapter Zcode：启动需要 apiKey（由 /api/zcode/start 的请求体带进来），
// 所以它的 Start 在注册表里返回「请用专用入口」，真正的启动走 apiZcodeStart。
type zcodeAdapter struct{ h *Hub }

func (a zcodeAdapter) Key() string { return "zcode" }

func (a zcodeAdapter) Running() bool {
	p := a.h.zcodeForAgg()
	return p != nil && p.Running()
}

func (a zcodeAdapter) Start() (bool, string) {
	return false, "Zcode 需要 apiKey，请用 /api/zcode/start 并在请求体传 {\"api_key\":\"...\"}"
}

func (a zcodeAdapter) Stop() (bool, string) { return a.h.stopZcode() }

func (a zcodeAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	if p := a.h.zcodeForAgg(); p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "Zcode 线路未初始化", "type": "line_unavailable"},
	})
}

func (a zcodeAdapter) NativeResponses() http.HandlerFunc { return nil }

func (a zcodeAdapter) Models() []string {
	out := make([]string, 0, len(zcodeModelAllowlist))
	for name := range zcodeModelAllowlist {
		out = append(out, name)
	}
	return out
}

// ocAdapter OpenCode 免费档：无需本机凭据。
type ocAdapter struct{ h *Hub }

func (a ocAdapter) Key() string { return "oc" }

func (a ocAdapter) Running() bool {
	p := a.h.ocForAgg()
	return p != nil && p.Running()
}

func (a ocAdapter) Start() (bool, string) { return a.h.startOpencode() }
func (a ocAdapter) Stop() (bool, string)  { return a.h.stopOpencode() }

func (a ocAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	if p := a.h.ocForAgg(); p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "OpenCode 线路未初始化", "type": "line_unavailable"},
	})
}

func (a ocAdapter) NativeResponses() http.HandlerFunc { return nil }

func (a ocAdapter) Models() []string {
	out := make([]string, 0, len(opencodeModelIDs))
	for name := range opencodeModelIDs {
		out = append(out, name)
	}
	return out
}

// mimoAdapter MiMo：SSO 通道，不参与聚合。
type mimoAdapter struct{ h *Hub }

func (a mimoAdapter) Key() string { return "mimo" }

func (a mimoAdapter) Running() bool {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	return a.h.mimo != nil && a.h.mimo.Running()
}

func (a mimoAdapter) Start() (bool, string) { return a.h.startMimo() }
func (a mimoAdapter) Stop() (bool, string)  { return a.h.stopMimo() }

func (a mimoAdapter) Chat(w http.ResponseWriter, r *http.Request) {
	a.h.mu.Lock()
	p := a.h.mimo
	a.h.mu.Unlock()
	if p != nil {
		p.handleChat(w, r)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": "MiMo 线路未初始化", "type": "line_unavailable"},
	})
}

func (a mimoAdapter) NativeResponses() http.HandlerFunc { return nil }
func (a mimoAdapter) Models() []string                  { return nil }

// lineAdapters 线路注册表：新增线路在这里加一行构造器。
var lineAdapters = map[string]func(*Hub) lineAdapter{
	"dom":       func(h *Hub) lineAdapter { return proxyLineAdapter{h: h, key: "dom"} },
	"intl":      func(h *Hub) lineAdapter { return proxyLineAdapter{h: h, key: "intl"} },
	"qoder":     func(h *Hub) lineAdapter { return qoderAdapter{h: h} },
	"trae":      func(h *Hub) lineAdapter { return traeAdapter{h: h} },
	"teleagent": func(h *Hub) lineAdapter { return teleagentAdapter{h: h} },
	"zcode":     func(h *Hub) lineAdapter { return zcodeAdapter{h: h} },
	"mimo":      func(h *Hub) lineAdapter { return mimoAdapter{h: h} },
	"oc":        func(h *Hub) lineAdapter { return ocAdapter{h: h} },
}

// adapterFor 取某条线路的适配器；未注册返回 false。
func (h *Hub) adapterFor(key string) (lineAdapter, bool) {
	factory, ok := lineAdapters[key]
	if !ok {
		return nil, false
	}
	return factory(h), true
}
