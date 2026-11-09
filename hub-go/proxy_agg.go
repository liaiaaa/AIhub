package main

// 聚合线路（:8790）：把多条线路的模型挂到同一个 OpenAI/Codex 兼容入口，
// 给 Codex 用（一个 provider 只能配一个 base_url）。用模型名前缀路由：
// slug 形如 intl.deepseek-v4.1-flash，前缀决定走哪条线路，请求体里的
// model 会剥掉前缀再转发。控制台可启停（默认关），不用 Codex 时不占内存。
//
// 转发方式分两类：
//   - dom/intl（WorkBuddy *Proxy）：进程内直调 handleResponses/handleChat；
//   - qoder/trae（独立协议，只有 chat/completions）：先把 Responses 请求
//     转成 chat 请求体（responsesBodyToChatBody）再直调其 handleChat。
//
// 白名单不写死在源码：存 dataPath("agg_models.json")，控制台
// 「Codex 模型管理」卡片点击增删，即时生效（Codex 端重开会话刷新列表）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const AggPort = 8790

// aggPort 是聚合口实际监听的端口。默认等于 AggPort；测试里改成别的值，
// 免得断言时撞上正在运行的正式实例（本机 Codex 就挂在 :8790 上）。
var aggPort = AggPort

type aggModel struct {
	Line    string `json:"line"`    // dom / intl / qoder / trae
	Slug    string `json:"slug"`    // 对外（客户端请求里）的模型名，首个 '.' 前即线路名
	Backend string `json:"backend"` // 转给具体线路的真实模型名
	Display string `json:"display"` // codex 模型选择器里显示的名字
}

var (
	aggMu    sync.Mutex
	aggList  []aggModel
	aggSrv   *http.Server // nil = 聚合线路未启动
	aggSrvMu sync.Mutex
	// aggServeDone 在 startAggLine 里创建、Serve goroutine 退出时 close。
	// stopAggLine 等它，确保「停止」返回时后台 goroutine 已经收尾
	// （那个 goroutine 最后会 logHub → 读全局 hubRoot，2026-10-19 修）。
	aggServeDone chan struct{}
)

// aggLineCN 线路中文名（错误提示 / 自动显示名用）；
// aggLinePort 各线路本地端口（拉取在线模型目录用，与 webui SERVICES 一致）。
// "agg" 不是真实线路，是「被聚合口拒掉的请求」的统计归属（如未知模型 400），
// 没有它用量表会显示成空名字（见 usage_stats.go 的 line_cn）。
var aggLineCN = map[string]string{"dom": "国内", "intl": "国际", "qoder": "Qoder", "trae": "Trae", "teleagent": "TeleAgent", "zcode": "Zcode", "oc": "OpenCode免费", "official": "官方", "agg": "聚合口(未受理)"}

// aggSelectableLine 判断是不是「可挂模型的真实线路」。
// 不能拿 aggLineCN 当校验表——它含 "agg" 这个纯统计用的键，
// 用它校验会让 /api/agg/add?line=agg 被误认为合法线路。
func aggSelectableLine(line string) bool {
	if lineHidden(line) {
		return false // 隐藏的线路不能再被加进聚合白名单
	}
	switch line {
	case "dom", "intl", "qoder", "trae", "teleagent", "zcode", "oc":
		return true
	}
	return false
}

// aggLinePort 参与聚合的线路 → 端口，来自 lineSpecs（lines.go）。
// 原来是手抄的字面量表，端口一变就得记得同步两条地方。
var aggLinePort = aggregatedLinePorts()

// aggCCSKeys 兼容 Hub Switch 时代的模型名前缀：ccs-<key>/<model> 与
// ccs-claude-<key>--<model>（2026-10-13 方案 C：hub-go 吞并网关后，
// 客户端里存量配置的模型名不用改也能继续路由）。
var aggCCSKeys = map[string]string{
	"dom": "dom", "intl": "intl", "qoder": "qoder", "trae": "trae", "zcode": "zcode",
	"claude-dom": "dom", "claude-intl": "intl", "claude-qoder": "qoder",
	"claude-trae": "trae", "claude-zcode": "zcode",
}

func aggDefaults() []aggModel {
	return []aggModel{
		{Line: "intl", Slug: "intl.deepseek-v4.1-flash", Backend: "deepseek-v4.1-flash", Display: "DeepSeek 4.1 Flash（国际）"},
		{Line: "dom", Slug: "dom.glm-5.3-flash", Backend: "glm-5.3-flash", Display: "GLM 5.3 Flash（国内）"},
	}
}

func aggLoad() {
	aggMu.Lock()
	defer aggMu.Unlock()
	b, err := os.ReadFile(dataPath("agg_models.json"))
	if err == nil && json.Unmarshal(b, &aggList) == nil && len(aggList) > 0 {
		aggNormalizeDisplayLocked()
		return
	}
	aggList = aggDefaults()
	aggSaveLocked()
}

// aggNormalizeDisplayLocked 把存量条目的 display 规范成友好名。
//
// 2026-10-15 新增：aggAdd 早先用 backend 直接拼显示名，历史白名单里留下
// kimi-k3（国际）这类小写英文 id。这里启动时把「正好等于旧规则产物」的条目
// 重算一遍；用户手工改过的显示名（不等于旧规则产物）原样保留，不覆盖。
func aggNormalizeDisplayLocked() {
	changed := false
	for i := range aggList {
		m := &aggList[i]
		legacy := m.Backend + "（" + aggLineCN[m.Line] + "）"
		if m.Display == legacy || m.Display == "" {
			if fresh := aggDisplayName(m.Line, m.Backend); fresh != m.Display {
				m.Display = fresh
				changed = true
			}
		}
	}
	if changed {
		aggSaveLocked()
		codexCatalogRefresh()
	}
}

// aggSaveLocked 必须在 aggMu 持锁时调用。
//
// 2026-11-07 改成 tmp+rename：原来直接 os.WriteFile 且把 err 丢掉，写到一半
// 被强杀/磁盘满就留半截 JSON，而 aggLoad 解析失败是静默回落默认清单的 ——
// 表现为「用户手选的目录模型全没了」且日志里查不到任何原因。
// 与 state.json（hub.go saveStateLocked）保持同一套口径：失败记日志、替换
// 用 rename，保证目标文件要么是旧的完整内容、要么是新的完整内容。
func aggSaveLocked() {
	b, err := json.MarshalIndent(aggList, "", "  ")
	if err != nil {
		logHub("agg_models.json 序列化失败：%v", err)
		return
	}
	path := dataPath("agg_models.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		logHub("agg_models.json 写入临时文件失败：%v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		logHub("agg_models.json 替换失败：%v", err)
		_ = os.Remove(tmp)
	}
}

func aggSnapshot() []aggModel {
	aggMu.Lock()
	defer aggMu.Unlock()
	// 2026-11-03：隐藏线路的模型不进聚合（否则 /v1/models 里还会列出它们，
	// 调用时又因为线路不能启动而失败）。
	hidden := hiddenLines()
	out := make([]aggModel, 0, len(aggList))
	for _, m := range aggList {
		if hidden[m.Line] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// aggRiskTag 高风险线路在模型显示名前的标记。
//
// 2026-10-23 上游收紧对非官方客户端的识别后，dom 是风险最高的线路：反代到
// dsh 这类 agent 框架时请求体大、工具多、system 里有身份句，正是上游抓的形状。
// 这里只加显示标记，不改模型 id 与路由——要不要用仍由用户决定。
const aggRiskTag = "⚠高风险 "

// aggRiskTagged 给高风险线路的显示名加前缀；已经带前缀则不重复加（幂等），
// 因为 Display 可能来自旧的 agg_models.json 快照、也可能刚由 aggDisplayName 生成。
func aggRiskTagged(line, name string) string {
	if line != "dom" || name == "" || strings.HasPrefix(name, aggRiskTag) {
		return name
	}
	return aggRiskTag + name
}

// aggDisplayName 生成白名单条目的显示名：`<友好名>（<线路中文名>）`。
//
// 2026-10-15 新增：原来直接用 backend（如 kimi-k3（国际）），选择器里是一串
// 小写英文 id。现在优先用友好名映射（各线路高频模型的通用写法），未命中的
// 回落 backend，保证新增模型也有可读的显示名。
func aggDisplayName(line, backend string) string {
	name := backend
	if n, ok := aggPrettyNames[backend]; ok {
		name = n
	}
	return aggRiskTagged(line, name+"（"+aggLineCN[line]+"）")
}

// aggPrettyNames 模型 id → 友好显示名（不含线路后缀）。
// 只收各线路目录里常见的高频模型；不求全，未列入的走 backend 原值。
var aggPrettyNames = map[string]string{
	// 国内 / 国际 WorkBuddy 共有
	"deepseek-v4.1-flash": "DeepSeek 4.1 Flash",
	"deepseek-v4-flash":   "DeepSeek V4 Flash",
	"deepseek-v4-pro":     "DeepSeek V4 Pro",
	"glm-5.3-flash":       "GLM 5.3 Flash",
	"glm-5.3":             "GLM 5.3",
	"glm-5.2":             "GLM 5.2",
	"glm-5.1":             "GLM 5.1",
	"glm-5v-turbo":        "GLM 5V Turbo",
	"hy4-preview":         "HY4 Preview",
	"hy3-preview":         "HY3 Preview",
	"hy3-preview-agent":   "HY3 Preview Agent",
	"kimi-k3":             "Kimi K3",
	"kimi-k2.8-preview":   "Kimi K2.8 Preview",
	"kimi-k2.6":           "Kimi K2.6",
	"kimi-k2.7":           "Kimi K2.7",
	"minimax-m3":          "MiniMax M3",
	"minimax-m3-pay":      "MiniMax M3",
	"space-bunny":         "Space Bunny",
	"space-bunny-max":     "Space Bunny Max",
	"step-5-preview":      "Step 5 Preview",
	// 国际线额外模型
	"grok-4.7":         "Grok 4.7",
	"gemini-3.8-flash": "Gemini 3.8 Flash",
	"gemini-3.5-flash": "Gemini 3.5 Flash",
	"gpt-5.4":          "GPT 5.4",
	"gpt-5.5":          "GPT 5.5",
	"gpt-5.6-luna":     "GPT 5.6 Luna",
	"gpt-5.6-terra":    "GPT 5.6 Terra",
	"gpt-5.6-sol":      "GPT 5.6 Sol",
	"gpt-6-astra":      "GPT 6 Astra",
	"gpt-6-luna":       "GPT 6 Luna",
	"gpt-6-sol":        "GPT 6 Sol",
	"gpt-6.1-sol":      "GPT 6.1 Sol",
	// Qoder 线（客户端可见的 DisplayName）
	"Qwen3.8-Flash":     "Qwen3.8 Flash",
	"Qwen3.8-Max":       "Qwen3.8 Max",
	"Qwen3.7-Plus":      "Qwen3.7 Plus",
	"Qwen3.7-Max":       "Qwen3.7 Max",
	"Qwen3.7-Flash":     "Qwen3.7 Flash",
	"DeepSeek-V4-Pro":   "DeepSeek V4 Pro",
	"DeepSeek-Flash":    "DeepSeek Flash",
	"GLM-5.3-Flash":     "GLM 5.3 Flash",
	"GLM-5.2":           "GLM 5.2",
	"Kimi-K3":           "Kimi K3",
	"Kimi-K2.8-Preview": "Kimi K2.8 Preview",
	"MiniMax-M2.7":      "MiniMax M2.7",
	"Auto":              "Auto",
	"Fast":              "Fast",
}

// aggAdd / aggRemove 的 codexCatalogRefresh 必须在**锁外**调用：
// 它起的 goroutine 要抢 aggMu 取白名单快照，持锁调用会让 /v1/models
// 在写目录期间排队（客户端表现为启动时偶发秒级卡顿）。2026-10-21 开源审查。
// 因此这两个函数用显式 Unlock 而不是 defer。
func aggAdd(line, backend string) (aggModel, bool, string) {
	aggMu.Lock()
	slug := line + "." + backend
	for _, m := range aggList {
		if m.Slug == slug {
			aggMu.Unlock()
			return m, false, "已存在：" + slug
		}
	}
	m := aggModel{Line: line, Slug: slug, Backend: backend, Display: aggDisplayName(line, backend)}
	aggList = append(aggList, m)
	aggSaveLocked()
	aggMu.Unlock()

	codexCatalogRefresh()
	return m, true, "已添加 " + slug
}

func aggRemove(slug string) (bool, string) {
	aggMu.Lock()
	for i, m := range aggList {
		if m.Slug == slug {
			aggList = append(aggList[:i], aggList[i+1:]...)
			aggSaveLocked()
			aggMu.Unlock()
			codexCatalogRefresh()
			return true, "已删除 " + slug
		}
	}
	aggMu.Unlock()
	return false, "不存在：" + slug
}

// ---------- 聚合线路启停 ----------

func (h *Hub) startAggLine() (bool, string) {
	aggSrvMu.Lock()
	defer aggSrvMu.Unlock()
	if aggSrv != nil {
		return true, "已在运行"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "models": len(aggSnapshot())})
	})
	// 目录可观测性：Codex 桌面版只在启动时拉一次模型目录，出问题时
	// （拉到 0 个 / 根本没来拉）从日志要能看出来。按客户端版本区分形态，
	// 每次请求记一行，不刷屏（桌面版启动时也就一两次）。
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		cv := r.URL.Query().Get("client_version")
		if cv != "" {
			out := h.aggCodexModels()
			n := len(asSlice(out["models"]))
			logHub("agg /v1/models 目录请求 | client_version=%s | codex 形态 | %d 个模型", cv, n)
			if n == 0 {
				logHub("agg ⚠ 模型目录为空：白名单里没有可用模型，客户端会回退到内置官方目录")
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		out := h.aggOpenAIModels()
		logHub("agg /v1/models 目录请求 | OpenAI 形态 | %d 个模型", aggOpenAIModelCount(out))
		writeJSON(w, http.StatusOK, out)
	})
	// Codex 的 model_catalog_url 形态（2026-11-03）：magpie 的做法是把目录放在
	// 独立路径上 —— 因为 /v1/models 是 OpenAI 的标准列表，Codex 读不了它。
	// 有了这个端点，别的机器上的 Codex 不用再拷 ~/.codex/aihub-model-catalog.json。
	mux.HandleFunc("/v1/codex/models", func(w http.ResponseWriter, r *http.Request) {
		out := h.aggCodexModels()
		logHub("agg /v1/codex/models 目录请求 | %d 个模型", len(asSlice(out["models"])))
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		h.aggDispatch(w, r, "responses")
	})
	// Codex 原生 responses 路径（A′ 方案）：config.toml 只留 openai_base_url 时
	// Codex 把 /responses 打到 /backend-api/codex/responses。GET 同样接受，
	// 因为部分客户端会用 GET 做重试探测。
	mux.HandleFunc("/backend-api/codex/responses", func(w http.ResponseWriter, r *http.Request) {
		h.codexBackendResponses(w, r)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		h.aggDispatch(w, r, "chat")
	})
	// Anthropic 入站（2026-10-13 方案 C）：Claude Code / Claude Desktop 3P 直连本口
	// /claude-desktop 前缀路径是 Desktop 网关 profile 的 base_url 形状（与
	// Hub Switch 时代一致），与 /v1/messages 同一 handler。
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		h.aggAnthropicMessages(w, r)
	})
	mux.HandleFunc("/claude-desktop/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		h.aggAnthropicMessages(w, r)
	})
	mux.HandleFunc("/claude-desktop/v1/models", func(w http.ResponseWriter, r *http.Request) {
		h.aggOpenAIModelsHandler(w, r)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", aggPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false, "聚合端口绑定失败（可能被残留进程占用）: " + err.Error()
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	aggSrv = srv
	// serveDone 让 stopAggLine 能等 Serve 的 goroutine 真正退出。
	// 2026-10-19：race detector 发现 Serve 返回后该 goroutine 仍会调 logHub
	// → dataPath 读 hubRoot，而 stopAggLine 不等它就返回了——停服后立刻读
	// 全局状态的调用方（测试、退出流程）会与这个尾巴打架。
	// 这是生产代码的真实时序缺陷，不只是测试问题。
	done := make(chan struct{})
	aggServeDone = done
	go func() {
		defer recoverGoroutine("proxy_agg.go:323")
		defer close(done)
		_ = srv.Serve(ln)
		aggSrvMu.Lock()
		if aggSrv == srv {
			aggSrv = nil
		}
		aggSrvMu.Unlock()
		logHub("agg proxy stopped")
	}()
	logHub("agg proxy started on %s（%d 个模型）", addr, len(aggSnapshot()))
	return true, fmt.Sprintf("聚合线路已启动（:%d，%d 个模型）", aggPort, len(aggSnapshot()))
}

func (h *Hub) stopAggLine() (bool, string) {
	aggSrvMu.Lock()
	srv := aggSrv
	done := aggServeDone
	aggSrv = nil
	aggServeDone = nil
	aggSrvMu.Unlock()
	if srv == nil {
		return true, "聚合已停止"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	// 等 Serve 的 goroutine 收尾（它最后会 logHub 一次，读全局 hubRoot）。
	// Shutdown 返回后 Serve 通常已返回，这里最多再等 2 秒兜底。
	if done != nil {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			logHub("agg Serve goroutine 未在 2s 内退出（继续，不影响停止结果）")
		}
	}
	return true, "聚合已停止"
}

// aggUserStopped 记录「本次运行内用户是否手动停过聚合口」。
// 手动停过就不再自启，尊重用户意图；否则 hub 启动/换凭证重启后自动监听。
var aggUserStopped bool

// aggAutostart 让聚合口随 hub 自动监听（2026-10-12 事故修复）。
//
// 背景：:8790 原先要手动点「启动聚合」才监听。而 Codex 桌面版只在启动时
// 读一次模型目录，正好落在空窗里就会回退到 codex 二进制内置的官方目录
// （gpt-5.6-sol 之类），且不会自愈——只能重启桌面版。今天 hub 重启 7 次，
// 每次 agg 晚 5~54 秒，实测踩中过。
//
// 白名单非空 = 用户已经配了要给 Codex 用的模型，自启是合理默认。
// 只豁免 :8790 这一个口，其余线路仍保持「启动后默认关闭」。
func (h *Hub) aggAutostart() {
	if len(aggSnapshot()) == 0 {
		logHub("agg 白名单为空，不自动监听 :%d", aggPort)
		return
	}
	aggSrvMu.Lock()
	stopped := aggUserStopped
	aggSrvMu.Unlock()
	if stopped {
		logHub("用户已手动停止过聚合口，本次运行不再自动监听")
		return
	}
	ok, msg := h.startAggLine()
	if ok {
		logHub("agg 自动监听：%s", msg)
	} else {
		logHub("agg 自动监听失败：%s", msg)
	}
}

// aggStatusEntry 供控制台卡片轮询：聚合口是否在跑 + 白名单模型数。
func aggStatusEntry() map[string]any {
	aggSrvMu.Lock()
	up := aggSrv != nil
	aggSrvMu.Unlock()
	return map[string]any{"up": up, "models": len(aggSnapshot())}
}

// ---------- 路由与分发 ----------

func (h *Hub) proxyForLine(line string) *Proxy {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch line {
	case "dom":
		return h.dom
	case "intl":
		return h.intl
	}
	return nil
}

func (h *Hub) qoderForAgg() *QoderProxy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.qoder
}

func (h *Hub) traeForAgg() *TraeProxy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trae
}

func (h *Hub) teleagentForAgg() *TeleAgentProxy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.teleagent
}

func (h *Hub) zcodeForAgg() *ZcodeProxy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.zcode
}

// ocForAgg OpenCode 免费档（2026-10-15）。
func (h *Hub) ocForAgg() *OpencodeProxy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.oc
}

func (h *Hub) lineRunning(line string) bool {
	ad, ok := h.adapterFor(line)
	if !ok {
		return false
	}
	return ad.Running()
}

// resolveAggThirdParty 只解析「第三方」模型：白名单 / ccs- / <线路>.<模型>。
// A′ 的 Codex 原生路径用它判定「是不是第三方」，从而不会把官方 slug 当成线路模型。
func resolveAggThirdParty(slug string) (aggModel, bool) {
	for _, am := range aggSnapshot() {
		if am.Slug == slug {
			return am, true
		}
	}
	// Claude Desktop 安全角色名（claude-sonnet-5 等，含日期后缀）→ 聚合 slug
	if s := resolveDesktopRole(slug); s != "" {
		if am, ok := resolveAggModel(s); ok {
			return am, true
		}
	}
	// ccs- 形式（含 claude 桌面版的 key--model 写法）
	if rest, ok := strings.CutPrefix(slug, "ccs-"); ok {
		var key, model string
		if i := strings.Index(rest, "/"); i >= 0 {
			key, model = rest[:i], rest[i+1:]
		} else if i := strings.Index(rest, "--"); i >= 0 {
			key, model = rest[:i], rest[i+2:]
		}
		if line, ok := aggCCSKeys[key]; ok && model != "" {
			return aggModel{Line: line, Slug: slug, Backend: model,
				Display: model + "（" + aggLineCN[line] + "）"}, true
		}
		return aggModel{}, false
	}
	// <线路>.<模型> 动态形式
	if line, model, found := strings.Cut(slug, "."); found && aggSelectableLine(line) && model != "" {
		return aggModel{Line: line, Slug: slug, Backend: model,
			Display: model + "（" + aggLineCN[line] + "）"}, true
	}
	return aggModel{}, false
}

// resolveAggModel 统一网关的模型名解析（2026-10-13 方案 C 升级）。
// 前三级与 resolveAggThirdParty 完全相同；最后一级把官方 gpt-* 解析成 official。
func resolveAggModel(slug string) (aggModel, bool) {
	if am, ok := resolveAggThirdParty(slug); ok {
		return am, true
	}
	// 官方 ChatGPT 模型（gpt-*）：透传官方 API（2026-10-13 共存方案，见 proxy_official.go）。
	if isOfficialModelSlug(slug) {
		return aggModel{Line: "official", Slug: slug, Backend: slug,
			Display: slug + "（官方）"}, true
	}
	return aggModel{}, false
}

// rewriteModel 把请求体里的聚合 slug 换成线路真实模型名。
func rewriteModel(raw []byte) (aggModel, []byte, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return aggModel{}, nil, fmt.Errorf("bad json: %w", err)
	}
	slug, _ := m["model"].(string)
	if am, ok := resolveAggModel(slug); ok {
		m["model"] = am.Backend
		nb, err := json.Marshal(m)
		return am, nb, err
	}
	return aggModel{}, nil, fmt.Errorf("未知模型 %q（可用形式：线路.模型 / ccs-线路/模型，或在控制台白名单里添加）", slug)
}

// requestedModelSlug 尽力取出请求体里的 model 字段（只为统计留痕，失败返回空）。
func requestedModelSlug(raw []byte) string {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return ""
	}
	s, _ := m["model"].(string)
	return strings.TrimSpace(s)
}

// responsesBodyToChatBody 把 Responses 请求体转成 chat/completions 请求体，
// 供只有 chat 协议的线路（qoder/trae）复用；messages 转换复用
// responsesInputToMessages（developer/system 文本的降敏也一并生效）。
// 思考档位只透传客户端显式值，默认档由各线路自己的 chat 路径处理。
func responsesBodyToChatBody(nb []byte) (map[string]any, error) {
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(nb))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("bad json: %w", err)
	}
	msgs := responsesInputToMessages(raw)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	body := map[string]any{}
	for _, k := range []string{"model", "temperature", "top_p", "stop", "parallel_tool_calls", "user", "verbosity"} {
		if v, ok := raw[k]; ok {
			body[k] = v
		}
	}
	body["messages"] = msgs
	if tools := responsesToolsToChat(raw["tools"]); tools != nil {
		body["tools"] = tools
	}
	if tc := responsesToolChoiceToChat(raw["tool_choice"]); tc != nil {
		body["tool_choice"] = tc
	}
	if rm, ok := raw["reasoning"].(map[string]any); ok {
		if eff, ok := rm["effort"].(string); ok && eff != "" {
			body["reasoning_effort"] = eff
		}
	}
	clientStream, _ := raw["stream"].(bool)
	body["stream"] = clientStream
	if clientStream {
		if _, ok := body["stream_options"]; !ok {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	return body, nil
}

// aggLineOff 线路没启动时的统一 503，提示去控制台拉起。
func aggLineOff(w http.ResponseWriter, am aggModel) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{"message": fmt.Sprintf("模型 %s 在 %s 线路上，但该线路未启动：请先在 AIHub 控制台启动它", am.Backend, aggLineCN[am.Line]), "type": "line_not_running"},
	})
}

// aggCall 合成新请求进程内直调线路 handler（不过 HTTP）。
func aggCall(w http.ResponseWriter, r *http.Request, path string, body []byte, handler func(http.ResponseWriter, *http.Request)) {
	req, err := http.NewRequestWithContext(r.Context(), "POST", path, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "internal_error"},
		})
		return
	}
	req.Header = r.Header.Clone()
	req.ContentLength = int64(len(body))
	handler(w, req)
}

// aggToChatOnly 只有 chat 协议的线路：chat 请求直调，responses 请求先转换。
// aggToChatOnly 处理「只有 chat/completions 协议的线路」（qoder/trae）的聚合请求。
//
// 两条路径都要做双向翻译，缺一不可：
//   - kind=chat：客户端本来就要 chat，直通即可；
//   - kind=responses：客户端（Codex，wire_api=responses）只认 Responses 形态，
//     而 qoder/trae 只会吐 chat 形态，所以请求侧要 Responses→chat、
//     **响应侧还要 chat→Responses 转回来**。
//
// 2026-10-12 修复：此前只做了请求侧转换，响应原样返回 chat 格式
// （{"object":"chat.completion","choices":[...]}）。Codex 解析不到 output[]
// 就认为没有内容——实测同一句话经 :8790 返回正文为空，直连 :8797 却正常。
// 转换复用 dom/intl 路径已有的 responsesStream（它本就吃标准 chat SSE）。
func aggToChatOnly(w http.ResponseWriter, r *http.Request, kind string, nb []byte, am aggModel, chat func(http.ResponseWriter, *http.Request), rid string, t0 time.Time) {
	if kind == "chat" {
		aggCall(w, r, "/v1/chat/completions", nb, chat)
		return
	}
	var rawReq map[string]any
	clientStream := false
	if json.Unmarshal(nb, &rawReq) == nil {
		clientStream, _ = rawReq["stream"].(bool)
	}
	cb, err := responsesBodyToChatBody(nb)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	// 上游一律用流式：responsesStream 逐帧转成 Responses 事件，
	// 这样流式/非流式客户端都能得到正确的 output[]。
	cb["stream"] = true
	if _, ok := cb["stream_options"]; !ok {
		cb["stream_options"] = map[string]any{"include_usage": true}
	}
	b, _ := json.Marshal(cb)

	// 用 recorder 截住 handleChat 的 chat 格式输出，再转写成 Responses。
	rec := &chatCapture{header: http.Header{}}
	aggCall(rec, r, "/v1/chat/completions", b, chat)

	if rec.status >= 400 || !strings.HasPrefix(rec.header.Get("Content-Type"), "text/event-stream") {
		// 出错时 handleChat 写的是 JSON 错误体，原样透传（保留状态码）
		for k, vs := range rec.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		w.WriteHeader(rec.status)
		w.Write(rec.body.Bytes())
		return
	}

	// 与 dom/intl 路径保持同一语义：客户端要流式就给 SSE，否则给一次性 JSON。
	// （此前这里无条件走 responsesStream，导致非流式客户端拿到 SSE——
	// 与 handleResponses 的行为不一致。）
	// logger 传 nil：responsesStream 只用它记一行完成日志，聚合口这一层的
	// 用量统计已由 usageTap 覆盖，不必重复记。
	body := bytes.NewReader(rec.body.Bytes())
	if clientStream {
		responsesStream(w, body, nil, rid, t0)
		return
	}
	col := collectStream(body)
	writeJSON(w, http.StatusOK, buildResponseObject(col, am.Backend))
}

// chatCapture 是一个只收集不发送的 ResponseWriter：用于把 handleChat 的
// chat 格式输出截下来，再转成 Responses 形态写给真正的客户端。
// 不实现 Flusher 是刻意的——handleChat 见到非 Flusher 就不会逐帧 flush，
// 所有帧都会落在 buf 里，转换阶段一次读全。
type chatCapture struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (c *chatCapture) Header() http.Header { return c.header }
func (c *chatCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *chatCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}

// aggDispatch 解析模型前缀 → 找到线路代理 → 改写 model 后按线路协议分发。
// kind: "responses"（Codex 等 Responses 客户端）| "chat"（OpenAI 兼容客户端）。
func (h *Hub) aggDispatch(w http.ResponseWriter, r *http.Request, kind string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "read body: " + err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	am, nb, err := rewriteModel(body)
	if err != nil {
		// 「未知模型」也要进统计：取样点在 rewriteModel 成功之后，所以这类
		// 请求原本整条不落 usage.jsonl。补一行 code=400、slug=客户端请求的名字，
		// 这样「客户端把官方模型名（gpt-5.6-sol 等）发过来了」一眼可见
		// （2026-10-12 事故：桌面版回退内置目录后请求全被这一层拒掉，
		// 但统计里毫无痕迹，排查时看不见）。
		if slug := requestedModelSlug(body); slug != "" {
			recordUsage(usageRec{Line: "agg", Slug: slug, Kind: kind, Code: http.StatusBadRequest})
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"},
		})
		return
	}
	// 用量取样：四条线路的 Codex 请求都必经这里，包一层即全覆盖，不用改各线路流式实现。
	tap := &usageTap{w: w}
	w = tap
	rid := randHex(4)
	t0 := time.Now()
	defer func() { recordTap(tap, am, kind, time.Since(t0).Milliseconds()) }()
	switch am.Line {
	case "official":
		// 官方 ChatGPT 透传（2026-10-13 共存方案）：只支持 Codex 的 responses 形态。
		if kind != "responses" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": officialChatUnsupported, "type": "invalid_request_error"},
			})
			return
		}
		h.officialPassthrough(w, r, nb)
	default:
		// 2026-11-02 A2 第二步：线路转发改为按注册表（line_adapter.go）走 ——
		// 新增线路只要实现 lineAdapter，这里不用再加 case。
		ad, ok := h.adapterFor(am.Line)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"message": "未知线路：" + am.Line, "type": "invalid_request_error"},
			})
			return
		}
		if !ad.Running() {
			aggLineOff(w, am)
			return
		}
		// 有原生 responses 的线路（目前 dom/intl）走直通，其余线路把
		// /v1/responses 翻译成 chat/completions 再转发。
		if native := ad.NativeResponses(); native != nil {
			if kind == "responses" {
				aggCall(w, r, "/v1/responses", nb, native)
			} else {
				aggCall(w, r, "/v1/chat/completions", nb, ad.Chat)
			}
			return
		}
		aggToChatOnly(w, r, kind, nb, am, ad.Chat, rid, t0)
	}
}

// aggCodexModels 聚合线路的 codex 模型目录（display_name 带线路标注与倍率，
// Codex 模型选择器里直接能看到 0.03x/免费 这类计费参考）。
//
// 2026-10-15：gpt-* 官方模型不再进目录（用户要求目录只放 hub-go 线路模型），
// 透传路由在 resolveAggModel 里保留，直接请求 gpt-* 仍走官方订阅。
func (h *Hub) aggCodexModels() map[string]any {
	list := aggSnapshot()
	entries := make([]codexModelEntry, 0, len(list))
	for _, am := range list {
		display := am.Display
		if r, ok := h.modelRatio(am.Line, am.Backend); ok {
			display += " · " + ratioLabel(r)
		}
		e := codexModelEntry{slug: am.Slug, display: display}
		// 2026-11-03：窗口/输出/档位按模型取值，不再让所有模型共用全局窗口。
		if m, ok := h.modelMeta(am.Line, am.Backend); ok {
			e.ctx = m.ContextWindow
			e.maxOut = m.MaxInput
		}
		if le, ok := h.effortsOf(am.Line, am.Backend); ok {
			e.levels = le.Levels
			e.defaultLevel = le.Default
		}
		entries = append(entries, e)
	}
	return codexModelsResponseFrom(entries)
}

// aggOpenAIModels 聚合线路的标准 OpenAI /v1/models 列表。
//
// 2026-10-15 改造：DSH 等客户端的模型发现解析器（dsh-llm-pi-ai）同时接受
// 标准 `data` 数组和富信息 `models` 对象，后者能读出 id / 显示名 / 上下文窗口 /
// 最大输出 token。原来的实现只输出 `id`=英文 slug + `name`="slug · 倍率"，
// 客户端选择器里就是一串 intl.deepseek-v4.1-flash，看不出线路归属和计价。
//
// 现在输出富信息形态：
//   - key/id  ：聚合 slug（请求侧唯一标识，不能改）
//   - name    ：中文显示名 + 倍率（DSH 的显示名只认 name/display_name/displayName，
//     倍率不在它解析的字段里，只能并进显示名）
//   - contextWindow / maxTokens：元数据（有则输出）
//
// gpt-* 官方模型不再进目录（2026-10-15 用户要求：目录里不该混官方模型），
// 但透传路由保留——想用官方订阅仍可直接请求 gpt-*。
// aggOpenAIModelCount 数 OpenAI 形态 /v1/models 响应里的模型条数，专供日志用。
//
// 2026-10-27 修：aggOpenAIModels 的 data 是 Go 原生 []map[string]any，而
// asSlice 只认 JSON 解析出来的 []any，类型断言必然失败 —— 于是这行日志永远
// 报「0 个模型」，与实际返回（14 条）对不上，排查时被它带偏过一轮。
func aggOpenAIModelCount(out map[string]any) int {
	switch d := out["data"].(type) {
	case []map[string]any:
		return len(d)
	default:
		return len(asSlice(out["data"]))
	}
}

func (h *Hub) aggOpenAIModels() map[string]any {
	list := aggSnapshot()
	data := make([]map[string]any, 0, len(list))
	for _, am := range list {
		entry := map[string]any{
			"id":       am.Slug,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "aihub-agg",
			"name":     aggOpenAIDisplayName(h, am),
		}
		// 2026-11-03：能力字段。客户端（Codex / DSH / 第三方）靠这些决定
		// 是否显示「思考档位」菜单、能不能发图片、能不能带工具。
		entry["type"] = "model"
		entry["supported_parameters"] = []string{"tools"}
		if modelSupportsImage(am.Line, am.Backend) {
			entry["modalities"] = map[string]any{"input": []string{"text", "image"}, "output": []string{"text"}}
		} else {
			entry["modalities"] = map[string]any{"input": []string{"text"}, "output": []string{"text"}}
		}
		if le, ok := h.effortsOf(am.Line, am.Backend); ok {
			levels := le.Levels
			srl := make([]map[string]any, 0, len(levels))
			for _, l := range levels {
				srl = append(srl, map[string]any{"effort": l})
			}
			entry["reasoning"] = true
			entry["supported_reasoning_levels"] = srl
			entry["reasoning_levels"] = levels
			if le.Default != "" {
				entry["default_reasoning_effort"] = le.Default
			}
		} else {
			entry["reasoning"] = false
		}
		if m, ok := h.modelMeta(am.Line, am.Backend); ok {
			if m.ContextWindow > 0 {
				entry["contextWindow"] = m.ContextWindow
				// 同一个值的两种写法：camelCase 是早期给自定义客户端看的，
				// snake_case 是 DSH 的 deepseek provider 解析时认的名字
				// （dsh-llm-deepseek 里 capacity(entry.context_window)）。
				// 只写 camelCase 时上下文窗口填不进客户端表单（2026-11-01 用户报）。
				entry["context_window"] = m.ContextWindow
				entry["max_input_tokens"] = m.ContextWindow
			}
			if m.MaxInput > 0 {
				entry["max_output_tokens"] = m.MaxInput
			}
		}
		data = append(data, entry)
	}
	return map[string]any{"object": "list", "data": data}
}

// aggOpenAIDisplayName 生成 OpenAI 形态的显示名：中文名 + 倍率 + 上下文。
// 优先用白名单里配的 Display（已含「（国内）/（国际）」归属），没有时用
// 模型名兜底；倍率与上下文只在查得到时追加，查不到不编。
//
// 2026-10-19 修复：原来只拼倍率、不拼上下文，聚合目录显示成
// 「DeepSeek 4.1 Flash（国际） · 免费」，而单线路目录是
// 「deepseek-v4.1-flash · 免费 · 上下文 1M」——用户报「聚合网关里看不出上下文」。
// 现在与单线路的 ratioName 对齐：有上下文数据就带上，没有就不带（宁缺不编）。
func aggOpenAIDisplayName(h *Hub, am aggModel) string {
	base := am.Display
	if base == "" {
		base = am.Backend
	}
	// 快照里的 Display 可能是加标记前生成的，这里再兜一次（幂等）。
	base = aggRiskTagged(am.Line, base)
	r, hasRatio := h.modelRatio(am.Line, am.Backend)
	ctx := 0
	if m, ok := h.modelMeta(am.Line, am.Backend); ok {
		ctx = m.ContextWindow
	}
	switch {
	case hasRatio && ctx > 0:
		return withCtx(base, ratioLabel(r), ctx)
	case hasRatio:
		return base + " · " + ratioLabel(r)
	case ctx > 0:
		// 没有倍率数据时不能借 withCtx（它会拼出「base ·  · 上下文 1M」的空档）
		return base + " · 上下文 " + humanTokens(ctx)
	}
	return base
}

// ---------- 控制台 API ----------

func (h *Hub) apiAggStart(w http.ResponseWriter, r *http.Request) {
	ok, msg := h.startAggLine()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiAggStop(w http.ResponseWriter, r *http.Request) {
	aggSrvMu.Lock()
	aggUserStopped = true // 用户意图：本次运行别再自动拉起
	aggSrvMu.Unlock()
	ok, msg := h.stopAggLine()
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

func (h *Hub) apiAggState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, aggStatusEntry())
}

func (h *Hub) apiAggModels(w http.ResponseWriter, r *http.Request) {
	list := aggSnapshot()
	ratios := map[string]any{}
	metas := map[string]any{}
	models := make([]map[string]any, 0, len(list))
	for _, am := range list {
		if v, ok := h.modelRatio(am.Line, am.Backend); ok {
			ratios[am.Slug] = ratioLabel(v)
		}
		if mj := metaJSON(h.modelMeta(am.Line, am.Backend)); mj != nil {
			metas[am.Slug] = mj
		}
		models = append(models, map[string]any{
			"line": am.Line, "line_cn": aggLineCN[am.Line], "slug": am.Slug,
			"backend": am.Backend, "display": am.Display,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models, "ratios": ratios, "meta": metas})
}

// mergeUnique 把 extra 里 base 没有的项追加到 base 后面（不排序、不去重 base 内部）。
// 返回合并结果与「是否追加过」。
func mergeUnique(base, extra []string) ([]string, bool) {
	out := append([]string(nil), base...)
	var appended bool
	for _, e := range extra {
		if e == "" || stringInSlice(out, e) {
			continue
		}
		out = append(out, e)
		appended = true
	}
	return out, appended
}

// aggLineModelsPayload 组装 /api/agg/line_models 的响应（倍率/元数据按 id 现算）。
func (h *Hub) aggLineModelsPayload(line string, models []string, displays map[string]string, source, note string) map[string]any {
	ratios := map[string]string{}
	metas := map[string]any{}
	names := map[string]string{}
	for _, id := range models {
		if v, ok := h.modelRatio(line, id); ok {
			ratios[id] = ratioLabel(v)
		}
		if mj := metaJSON(h.modelMeta(line, id)); mj != nil {
			metas[id] = mj
		}
		if dn := displays[id]; dn != "" {
			names[id] = dn
		}
	}
	out := map[string]any{"ok": true, "models": models, "ratios": ratios, "meta": metas,
		"line_cn": aggLineCN[line], "source": source}
	if len(names) > 0 {
		out["display"] = names
	}
	if note != "" {
		out["note"] = note
	}
	return out
}

// probeLineModelIDs 取某条线路的上游模型 id（与「探测新模型」同一个数据源）。
// 会打上游，调用方自己把握频率；失败返回空切片（调用方回落）。
//
// 做成包级变量是为了单测能注入假上游结果 —— 否则这条路径只能靠真网络测，
// 而它恰恰是「线路没启动也要能加模型」的关键分支。
var probeLineModelIDs = func(h *Hub, line string) ([]string, map[string]string) {
	pl := probeLine(h, line)
	if !pl.Up || len(pl.Models) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(pl.Models))
	displays := map[string]string{}
	for _, m := range pl.Models {
		if m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
		if m.Display != "" && m.Display != m.ID {
			displays[m.ID] = m.Display
		}
	}
	return ids, displays
}

func (h *Hub) apiAggAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Line  string `json:"line"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "bad json: " + err.Error()})
		return
	}
	req.Line = strings.TrimSpace(req.Line)
	req.Model = strings.TrimSpace(req.Model)
	if !aggSelectableLine(req.Line) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "未知线路：" + req.Line})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "模型名不能为空"})
		return
	}
	_, added, msg := aggAdd(req.Line, req.Model)
	writeJSON(w, http.StatusOK, map[string]any{"ok": added, "msg": msg})
}

func (h *Hub) apiAggRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "bad json: " + err.Error()})
		return
	}
	ok, msg := aggRemove(strings.TrimSpace(req.Slug))
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "msg": msg})
}

// apiAggLineModels 拉某条线路可加入目录的模型清单。
//
// 2026-11-05 修一个用户报的死角：「探测新模型能探到 hy4，但目录模型那边加不了」。
// 根因是两条路径的数据源不一样 ——
//   - 探测（/api/models/probe）直接打**上游目录**，线路没启动也能出结果；
//   - 添加下拉（本接口）只读**线路自己的 /v1/models**，线路没跑就返回
//     ok=false「请先启动它」。于是探测看到的模型，在下拉里根本不存在。
//
// 现在：线路在跑 → 用线路目录打底，再并上上游探测里多出来的 id；
// 线路没跑 → 直接回上游探测结果（source=upstream），不再要求先启动线路。
func (h *Hub) apiAggLineModels(w http.ResponseWriter, r *http.Request) {
	line := r.URL.Query().Get("line")
	if !aggSelectableLine(line) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "msg": "未知线路：" + line})
		return
	}
	if !h.lineRunning(line) {
		ids, displays := probeLineModelIDs(h, line)
		if len(ids) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "models": []any{},
				"msg": "线路未启动，且上游目录也没取到模型（可先启动线路再试）",
			})
			return
		}
		writeJSON(w, http.StatusOK, h.aggLineModelsPayload(line, ids, displays, "upstream",
			"线路未启动：下面是上游实时目录，可直接加入（加入只是写进目录，真正调用前要先在上方启动这条线路）"))
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", aggLinePort[line]))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "线路端口无响应：" + err.Error(), "models": []any{}})
		return
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "模型列表解析失败", "models": []any{}})
		return
	}
	models := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			models = append(models, d.ID)
		}
	}
	// 并上上游目录里多出来的模型：探测能看到、线路目录还没广告出去的（新模型），
	// 不加这一段就还是「探得到但加不了」。
	ids, displays := probeLineModelIDs(h, line)
	merged, appended := mergeUnique(models, ids)
	source := "line"
	if appended {
		source = "line+upstream"
	}
	models = merged
	writeJSON(w, http.StatusOK, h.aggLineModelsPayload(line, models, displays, source, ""))
}
