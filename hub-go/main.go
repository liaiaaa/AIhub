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
	aggLoad() // 聚合线路白名单
	// 聚合口（:8790）随 hub 自动监听——它是 Codex 的入口，空窗期会让桌面版
	// 拉不到模型目录而回退到内置官方目录（2026-10-12 事故：54 秒空窗导致
	// 「选择模型」里出现 gpt-5.6-sol 等官方模型，且目录只在启动时读一次、
	// 不会自愈）。只豁免这一个口：其余线路仍保持「启动后默认关闭」的用户偏好。
	// 用户若在控制台手动停止聚合口，本次运行内不再自动拉起（见 aggUserStopped）。
	hub.aggAutostart()
	mux := http.NewServeMux()
	mux.HandleFunc("/", hub.handleRoot)
	mux.HandleFunc("/api/status", hub.apiStatus)
	mux.HandleFunc("/api/nick", hub.apiNick)
	mux.HandleFunc("/api/dom/start", hub.apiDomStart)
	mux.HandleFunc("/api/dom/stop", hub.apiDomStop)
	mux.HandleFunc("/api/intl/start", hub.apiIntlStart)
	mux.HandleFunc("/api/intl/stop", hub.apiIntlStop)
	mux.HandleFunc("/api/mimo/start", hub.apiMimoStart)
	mux.HandleFunc("/api/mimo/stop", hub.apiMimoStop)
	mux.HandleFunc("/api/qoder/start", hub.apiQoderStart)
	mux.HandleFunc("/api/qoder/stop", hub.apiQoderStop)
	mux.HandleFunc("/api/trae/start", hub.apiTraeStart)
	mux.HandleFunc("/api/trae/stop", hub.apiTraeStop)
	mux.HandleFunc("/api/zcode/start", hub.apiZcodeStart)
	mux.HandleFunc("/api/zcode/stop", hub.apiZcodeStop)
	mux.HandleFunc("/api/zcode/key", hub.apiZcodeKey)
	mux.HandleFunc("/api/intl/cred", hub.apiIntlCred)
	mux.HandleFunc("/api/intl/switch", hub.apiIntlSwitch)
	mux.HandleFunc("/api/checkin", hub.apiCheckin)
	mux.HandleFunc("/api/intl/checkin", hub.apiIntlCheckin)
	mux.HandleFunc("/api/intl/creds", hub.apiIntlCreds)
	mux.HandleFunc("/api/credits", hub.apiCredits)
	mux.HandleFunc("/api/usage", hub.apiUsage)
	mux.HandleFunc("/api/stop_all", hub.apiStopAll)
	mux.HandleFunc("/api/exit", hub.apiExit)
	mux.HandleFunc("/api/agg/state", hub.apiAggState)
	mux.HandleFunc("/api/agg/start", hub.apiAggStart)
	mux.HandleFunc("/api/agg/stop", hub.apiAggStop)
	mux.HandleFunc("/api/agg/models", hub.apiAggModels)
	mux.HandleFunc("/api/agg/add", hub.apiAggAdd)
	mux.HandleFunc("/api/agg/remove", hub.apiAggRemove)
	mux.HandleFunc("/api/agg/line_models", hub.apiAggLineModels)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
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
	select {}
}
