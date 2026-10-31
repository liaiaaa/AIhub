package main

// lineport.go 的行为测试（2026-10-31 A1 抽取时补）。
//
// 只测「成功绑定 → 优雅关闭 → 端口释放」与「端口仍占着时 waitPortFree 不误报」。
// **故意不测绑定失败路径**：listenLinePort 在绑定失败时会 stopPort 清扫占用者，
// 而 stopPort 是按端口找 PID 再 kill（platform.go:22）。测试里占着端口的正是当前
// 测试进程自己 —— 一测就会把自己杀掉（这也正是审计标出的真实风险，
// 见 REFACTOR 计划里「启动 SIGKILL 占用端口进程」一项，留待单独治理）。

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// freePort 复用 qoder_e2e_live_test.go:27 的同名助手（向系统借一个空闲端口）。

// TestListenLinePortBindAndClose 验证绑定成功、端口确实在监听、关闭后端口释放。
func TestListenLinePortBindAndClose(t *testing.T) {
	port := freePort(t)

	ln, err := listenLinePort(port)
	if err != nil {
		t.Fatalf("listenLinePort(%d) 应成功，实际报错: %v", port, err)
	}
	if !portListening(port) {
		t.Fatalf("绑定成功后端口 %d 应处于监听状态", port)
	}

	srv := newLineServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	go func() { _ = srv.Serve(ln) }()

	closeLineServer(srv, ln)
	if !waitPortFree(port, lineShutdownGrace) {
		t.Fatalf("closeLineServer 之后端口 %d 仍被占用", port)
	}
}

// TestWaitPortFreeTimeout 验证端口仍被监听时 waitPortFree 返回 false（不误报已释放）。
func TestWaitPortFreeTimeout(t *testing.T) {
	port := freePort(t)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("占住端口 %d 失败: %v", port, err)
	}
	defer func() { _ = ln.Close() }()

	if waitPortFree(port, 300*time.Millisecond) {
		t.Fatalf("端口 %d 仍在监听，waitPortFree 却报已释放", port)
	}
}

// TestListenLinePortConstants 钉住抽取前的数值：重试 4 次、间隔 500ms、
// 读头超时 20s、收尾 3s、轮询 100ms。这些数字直接决定启动/停止手感，改动要显式。
func TestListenLinePortConstants(t *testing.T) {
	if lineBindAttempts != 4 {
		t.Fatalf("lineBindAttempts = %d，抽取前是 4", lineBindAttempts)
	}
	if lineBindBackoff != 500*time.Millisecond {
		t.Fatalf("lineBindBackoff = %v，抽取前是 500ms", lineBindBackoff)
	}
	if lineReadHeaderTimeout != 20*time.Second {
		t.Fatalf("lineReadHeaderTimeout = %v，抽取前是 20s", lineReadHeaderTimeout)
	}
	if lineShutdownGrace != 3*time.Second {
		t.Fatalf("lineShutdownGrace = %v，抽取前是 3s", lineShutdownGrace)
	}
	if linePollInterval != 100*time.Millisecond {
		t.Fatalf("linePollInterval = %v，抽取前是 100ms", linePollInterval)
	}
}
