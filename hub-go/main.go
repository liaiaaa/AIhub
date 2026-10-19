package main

// AIHub (Go 版) — 单进程：
//   - 控制台（网页，127.0.0.1:8801）：状态 / 启停 / 换凭证 / 退出
//   - 两个代理（国内 8787 / 国际 8799）在进程内按需启停
// 启动默认全部停止：先确认控制台端口可用，再清扫 8787/8799 残留监听。

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	DomPort  = 8787
	IntlPort = 8799
	HubPort  = 8801

	defaultIntlCred = "<凭证目录B>"
)

// hubRoot（数据根目录：凭证/日志/state.json 的所在处）见 paths.go。
// 它不再硬绑 exe 位置，解析规则与覆盖方式也一并写在那里。

func main() {
	port := flag.Int("port", HubPort, "控制台端口")
	noBrowser := flag.Bool("no-browser", false, "不自动打开浏览器")
	flag.Parse()

	// 先确认控制台端口能否绑定，再决定要不要清扫代理端口。
	// 顺序很重要：早期实现先 sweepPorts 再绑定，导致「重复双击 exe」时
	// 第二个实例会把第一个实例正在运行的代理端口 taskkill 掉（等于把在跑的服务打掉）。
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		// 已有实例在跑：只打开浏览器
		openBrowser(*port)
		return
	}

	// 清扫代理端口残留：仅当本实例使用默认控制台端口（8801）时执行。
	// 用 -port 指定其他端口的是测试/第二实例（如预览），绝不能清扫——
	// 否则会把正在跑的正式实例的代理误杀（2026-10-08 预览事故根因）。
	if *port == HubPort {
		sweepPorts()
	}

	hub := NewHub()
	sharedHub = hub // 接管函数生成桌面版目录文件时用（codex_catalog.go）
	aggLoad()       // 聚合线路白名单
	// 统一网关 :8790（2026-10-13 方案 C）：Codex（/v1/responses）、Claude
	// （/v1/messages）、OpenAI（/v1/chat/completions）的聚合入口。
	// 按用户要求（2026-10-15）：启动即自动监听（白名单非空且本次运行没被
	// 手动停过）；各线路仍默认手动开，想用哪条在控制台自己启。
	hub.aggAutostart()
	mux := http.NewServeMux()
	mux.HandleFunc("/", hub.handleRoot)
	mux.HandleFunc("/api/status", hub.apiStatus)
	mux.HandleFunc("/api/nick", hub.apiNick)
	mux.HandleFunc("/api/dom/start", hub.apiDomStart)
	mux.HandleFunc("/api/dom/stop", hub.apiDomStop)
	mux.HandleFunc("/api/dom/cred", hub.apiDomCred)
	mux.HandleFunc("/api/dom/switch", hub.apiDomSwitch)
	mux.HandleFunc("/api/models/probe", hub.apiModelsProbe)
	mux.HandleFunc("/api/intl/start", hub.apiIntlStart)
	mux.HandleFunc("/api/intl/stop", hub.apiIntlStop)
	mux.HandleFunc("/api/mimo/start", hub.apiMimoStart)
	mux.HandleFunc("/api/mimo/stop", hub.apiMimoStop)
	mux.HandleFunc("/api/qoder/start", hub.apiQoderStart)
	mux.HandleFunc("/api/qoder/stop", hub.apiQoderStop)
	mux.HandleFunc("/api/trae/start", hub.apiTraeStart)
	mux.HandleFunc("/api/trae/stop", hub.apiTraeStop)
	mux.HandleFunc("/api/oc/start", hub.apiOpencodeStart)
	mux.HandleFunc("/api/oc/stop", hub.apiOpencodeStop)
	mux.HandleFunc("/api/zcode/start", hub.apiZcodeStart)
	mux.HandleFunc("/api/zcode/stop", hub.apiZcodeStop)
	mux.HandleFunc("/api/zcode/key", hub.apiZcodeKey)
	mux.HandleFunc("/api/intl/cred", hub.apiIntlCred)
	mux.HandleFunc("/api/intl/switch", hub.apiIntlSwitch)
	mux.HandleFunc("/api/checkin", hub.apiCheckin)
	mux.HandleFunc("/api/intl/checkin", hub.apiIntlCheckin)
	mux.HandleFunc("/api/intl/creds", hub.apiIntlCreds)
	mux.HandleFunc("/api/dom/creds", hub.apiDomCreds)
	mux.HandleFunc("/api/ctx/window", hub.apiCtxWindow)
	mux.HandleFunc("/api/clients/takeover", hub.apiClientsTakeover)
	mux.HandleFunc("/api/clients/status", hub.apiClientsStatus)
	mux.HandleFunc("/api/ratios", hub.apiRatios)
	mux.HandleFunc("/api/clients/restore", hub.apiClientsRestore)
	mux.HandleFunc("/api/credits", hub.apiCredits)
	mux.HandleFunc("/api/usage", hub.apiUsage)
	mux.HandleFunc("/api/gateway", hub.apiGateway)
	mux.HandleFunc("/api/stop_all", hub.apiStopAll)
	mux.HandleFunc("/api/exit", hub.apiExit)
	mux.HandleFunc("/api/agg/state", hub.apiAggState)
	mux.HandleFunc("/api/agg/start", hub.apiAggStart)
	mux.HandleFunc("/api/agg/stop", hub.apiAggStop)
	mux.HandleFunc("/api/agg/models", hub.apiAggModels)
	mux.HandleFunc("/api/agg/add", hub.apiAggAdd)
	mux.HandleFunc("/api/agg/remove", hub.apiAggRemove)
	mux.HandleFunc("/api/agg/line_models", hub.apiAggLineModels)

	srv := &http.Server{Handler: withCORS(mux), ReadHeaderTimeout: 20 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logHub("console serve error: %v", err)
		}
	}()
	logHub("hub started on 127.0.0.1:%d", *port)
	if !*noBrowser {
		openBrowser(*port)
	}
	// 自动签到已停用（2026-10-08）：自动定时签到有机器行为风控顾虑，
	// 签到改由控制台「一键签到」手动触发，详见 checkin.go。
	// 上游模型目录后台抓一次：/v1/models 的倍率+上下文靠它，不能等用户点控制台。
	go hub.warmWBCatalogs()
	select {}
}

// withCORS 控制台 API 的跨域放行（2026-10-13 融合改造）：
// Hub Switch 桌面壳（tauri:// 或 http://localhost 来源）的「线路与凭证」页
// 直接 fetch :8801 API。内核只听 127.0.0.1 且无鉴权，跨域头不扩大攻击面。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
