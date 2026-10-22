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
	"strings"
	"time"
)

const (
	DomPort  = 8787
	IntlPort = 8799
	HubPort  = 8801

	// 默认国际凭证目录名。2026-10-21 开源审查：原来写死作者本机的
	// 「<凭证目录B>」，既公开个人称呼，又让陌生人的 state.json 指向不存在的
	// 目录、首次启动就异常。改为空 —— 由用户在控制台自己选。
	defaultIntlCred = ""
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
	// 线路自动恢复（2026-10-19）：把上次运行过的线路重新拉起来。
	// 背景：2026-10-19 13:31 重启后只起了 agg+hub，dom/intl/qoder 全丢，
	// qoder 挂了很久才被发现（聚合网关里两个 qoder 模型持续 503）。
	// 根因是「线路在跑」这个事实只存在于内存，进程一退就丢，重启时无从得知
	// 该恢复哪些。现在由 state.json 的 last_lines 记住，这里异步恢复，
	// 不阻塞控制台起来。用户主动停掉的线路会被 forgetLine 摘掉，不会自己回来。
	go hub.restoreLines()
	// 启动时自动刷新一次额度（2026-10-20 用户要求）：以前要等前端轮询或手动点
	// 「刷新额度」才能看到余额/到期，现在起来就拉。异步不阻塞控制台；
	// creditsSnapshot(force=true) 清五平台缓存，domCredsStatus(force=true)
	// 清凭证卡片缓存（含积分到期，见 wbCreditExpiries）。
	go func() {
		time.Sleep(2 * time.Second)
		creditsSnapshot(hub, true)
		domCredsStatus(hub, true)
		logHub("启动额度已自动刷新（余额 + 凭证状态 + 积分到期）")
	}()
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

	srv := &http.Server{Handler: withConsoleGuard(*port)(mux), ReadHeaderTimeout: 20 * time.Second}
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

// consoleWriteOnly 只允许 POST 的控制台端点（防「GET 就能改状态」）。
//
// 2026-10-21 开源审查加固。此前 main.go 里这句注释是**错的**：
//
//	「内核只听 127.0.0.1 且无鉴权，跨域头不扩大攻击面」
//
// CORS 只决定「能不能读到响应」，不决定「请求能不能发出去」。任意网页
// （包括被注入广告的页面）都能：
//   - fetch('http://127.0.0.1:8801/api/exit', {method:'POST', mode:'no-cors'})
//     → 把所有人的 aihub 停掉
//   - <img src="http://127.0.0.1:8801/api/stop_all">（GET 也生效）
//     → 静默停服，连 JS 都不用
//   - POST /api/clients/takeover → 改写用户的 ~/.codex/config.toml 和
//     ~/.claude/settings.json
//   - DNS rebinding（代码不校验 Host）→ 连响应都能读走
//
// 这里做三件事：① 校验 Host 必须是本机（防 rebinding）；② 有 Origin 就必须是
// 本机来源（防跨站读写）；③ 状态变更端点只收 POST（挡掉 <img>/<script> 这类
// 无需 JS 的 GET 触发）。CORS 收窄成回显本机来源，不再发 `*`。
var consoleWriteOnly = []string{
	"/api/exit", "/api/stop_all",
	"/api/agg/start", "/api/agg/stop", "/api/agg/add", "/api/agg/remove",
	"/api/clients/takeover", "/api/clients/restore", "/api/clients/apply",
	"/api/ratios", "/api/ctx", "/api/checkin", "/api/stop_all",
}

func isConsoleWritePath(path string) bool {
	for _, p := range consoleWriteOnly {
		if path == p {
			return true
		}
	}
	// /api/<line>/{start,stop,switch,cred} 这类动态路径
	if strings.HasPrefix(path, "/api/") {
		for _, suf := range []string{"/start", "/stop", "/switch", "/takeover", "/restore", "/apply", "/claim", "/refresh"} {
			if strings.HasSuffix(path, suf) {
				return true
			}
		}
	}
	return false
}

// withConsoleGuard 控制台 API 的本机来源校验与 CORS 收窄。
func withConsoleGuard(port int) func(http.Handler) http.Handler {
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
		// IPv6 环回
		fmt.Sprintf("[::1]:%d", port): true,
	}
	origins := map[string]bool{
		fmt.Sprintf("http://127.0.0.1:%d", port): true,
		fmt.Sprintf("http://localhost:%d", port): true,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// ① Host 校验：挡住 DNS rebinding（攻击者域名解析到 127.0.0.1）
			if !hosts[r.Host] {
				http.Error(w, "forbidden: unexpected Host", http.StatusForbidden)
				return
			}
			// ② Origin 校验：挡住跨站请求（同源页面通常不带 Origin，
			//    带了就必须是本机；Hub Switch 桌面壳的 tauri:// 是自定义
			//    来源，它读的是路径不含 /api 的页面，这里放行非 http 来源）
			if o := r.Header.Get("Origin"); o != "" {
				if !origins[o] && (strings.HasPrefix(o, "http://") || strings.HasPrefix(o, "https://")) {
					http.Error(w, "forbidden: cross-origin", http.StatusForbidden)
					return
				}
			}
			// ③ 状态变更只收 POST：挡掉 <img src> / <script src> 这类
			//    无需 JS 就能发出的 GET
			if r.Method != http.MethodPost && isConsoleWritePath(r.URL.Path) {
				http.Error(w, "forbidden: write endpoint requires POST", http.StatusMethodNotAllowed)
				return
			}

			if o := r.Header.Get("Origin"); origins[o] {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
