package main

// 线路端口生命周期（2026-10-31 抽取）。
//
// 背景：5 条线路（dom/intl 走 Proxy、qoder、trae、zcode、opencode）原先各自复制了
// 同一份「绑定端口 → 失败清扫 → 500ms×4 重试」与「优雅关闭 → 强制断开 → 轮询确认
// 端口释放」的代码。几处逐字相同，改一处就容易漏四处。
//
// 这里只收敛机械部分：重试次数、间隔、超时与**错误文案仍留在调用方**，
// 因此行为与抽取前完全一致，唯一变化是少了几份副本。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// lineBindAttempts 端口绑定尝试次数（含首次）。抽取前是 for attempt := 0; attempt < 4。
	lineBindAttempts = 4
	// lineBindBackoff 两次绑定尝试之间的等待。
	lineBindBackoff = 500 * time.Millisecond
	// lineReadHeaderTimeout 各线路 http.Server 的读头超时。
	lineReadHeaderTimeout = 20 * time.Second
	// lineShutdownGrace 停止时给在途请求的收尾时间，超时则强制断开。
	lineShutdownGrace = 3 * time.Second
	// linePollInterval 确认端口是否已释放的轮询间隔。
	linePollInterval = 100 * time.Millisecond
)

// listenLinePort 在 127.0.0.1:port 上绑定监听。
//
// 绑定失败时先 stopPort 清扫占用者（上一次会话残留的 TIME_WAIT、还没完全退出的
// 同名进程），等 lineBindBackoff 再重试，最多 lineBindAttempts 次；全部失败时
// 返回最后一次错误。调用方负责把它翻译成给人看的提示。
func listenLinePort(port int) (net.Listener, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var ln net.Listener
	var err error
	for attempt := 0; attempt < lineBindAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(lineBindBackoff)
		}
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
		stopPort(port)
	}
	return nil, err
}

// newLineServer 构造各线路共用的 http.Server（只设读头超时，与抽取前一致）。
func newLineServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: lineReadHeaderTimeout}
}

// closeLineServer 优雅关闭线路：先给在途请求 lineShutdownGrace 收尾，
// 超时则强制断开，最后关 listener，保证端口一定释放。
//
// 只关自己的 listener，绝不按端口 PID 去杀进程——代理是进程内的，
// 那个 PID 往往就是 hub 自己。
func closeLineServer(srv *http.Server, ln net.Listener) {
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), lineShutdownGrace)
		_ = srv.Shutdown(ctx) // 先给在途请求一点时间收尾
		cancel()
		_ = srv.Close() // 超时则强制断开，保证端口一定释放
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// waitPortFree 轮询直到端口不再监听，最多等 timeout。只做端口确认，不杀进程。
// 返回端口最终是否已释放。
func waitPortFree(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portListening(port) {
			return true
		}
		time.Sleep(linePollInterval)
	}
	return !portListening(port)
}
