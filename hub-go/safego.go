package main

// 后台 goroutine 的 panic 兜底（2026-10-31 A6）。
//
// 现状：全仓 22 处 `go func`，一个 recover 都没有。Go 的 net/http 只兜住
// 「HTTP handler 里」的 panic（conn.serve 内部有 recover，会打日志并关连接），
// 但**不是** handler 派生出来的后台 goroutine，也不是签到/额度/看门狗这类
// 独立后台任务。这些地方一旦 panic，整个进程直接退出 —— 8801 控制台、
// 8790 聚合口与全部代理端口一起消失。
//
// 而承载本进程的 systemd transient 单元（scripts/start.sh 用 systemd-run 创建）
// **没有 Restart=**，也就是死了不会自愈，只能人工再 start。
// 一次 panic 就断掉所有客户端，代价与「少一个后台任务」完全不对等。
//
// 修法：每个后台 goroutine 第一条语句 `defer recoverGoroutine("<文件:行>")`，
// panic 时记日志（带堆栈）并让这个 goroutine 退出，进程继续服务。
// 用 recover 而不是「把 body 抽成函数再包 goSafe」，是因为 22 处里有 7 处
// 带着循环变量参数（go func(i int, key string)），改成闭包捕获会改变捕获语义，
// 而加一行 defer 是纯增量、语义零变化。

import (
	"runtime/debug"
)

// recoverGoroutine 在后台 goroutine 的 defer 里兜住 panic。
// name 用 "<文件>:<行>" 标出位置，日志里能直接定位。
func recoverGoroutine(name string) {
	if rec := recover(); rec != nil {
		logHub("后台任务 panic（%s）：%v\n%s", name, rec, debug.Stack())
	}
}
