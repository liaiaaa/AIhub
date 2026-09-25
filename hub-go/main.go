package main

// WorkBuddy Hub (Go 版) — 单进程：
//   - 控制台（网页，127.0.0.1:8801）：状态 / 启停 / 换凭证 / 退出
//   - 两个代理（国内 8787 / 国际 8799）在进程内按需启停
// 启动默认全部停止：先确认控制台端口可用，再清扫 8787/8799 残留监听。

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
	"flag"
)

// Windows API: 隐藏控制台窗口
var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
)

const SW_HIDE = 0 // 隐藏窗口

func hideConsoleWindow() {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, uintptr(SW_HIDE))
	}
}

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

	// 默认全部停止：清扫 8787/8799 残留监听（连续约 6 秒，覆盖启动窗口期）
	sweepPorts()

	hub := NewHub()
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
	mux.HandleFunc("/api/intl/cred", hub.apiIntlCred)
	mux.HandleFunc("/api/intl/switch", hub.apiIntlSwitch)
	mux.HandleFunc("/api/stop_all", hub.apiStopAll)
	mux.HandleFunc("/api/exit", hub.apiExit)

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
	select {}
}



