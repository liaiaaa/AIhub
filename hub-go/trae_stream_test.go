package main

// 2026-10-27 用户报：用 aihub 反代的 trae kimi-k3，dsh 会话等一分钟没消息。
//
// 排查结论：trae 的 kimi-k3 是思考模型（先吐 reasoning_content，正文要等思考
// 结束），上游那次请求 141s 只产出 581 token；同时 hub 原先没有任何兜底 ——
// 上游静默就无限等，客户端断开后上游还在跑（白烧额度）。
//
// 这条测试钉住三件事：
//  1. 上游连续无数据到阈值 → 主动中止并回明确的错误帧；
//  2. 客户端断开（写失败）→ 立刻停，不再继续读上游；
//  3. 正常流不受影响。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTraeIdleTimeoutEnv(t *testing.T) {
	t.Setenv(traeIdleEnv, "")
	if got := traeIdleTimeout(); got != traeIdleDefault {
		t.Errorf("默认应为 %s，实际 %s", traeIdleDefault, got)
	}
	t.Setenv(traeIdleEnv, "5")
	if got := traeIdleTimeout(); got != 5*time.Second {
		t.Errorf("环境变量 5 应为 5s，实际 %s", got)
	}
	for _, v := range []string{"abc", "0", "-3", "  "} {
		t.Setenv(traeIdleEnv, v)
		if got := traeIdleTimeout(); got != traeIdleDefault {
			t.Errorf("非法值 %q 应回落默认值，实际 %s", v, got)
		}
	}
}

// ctxBlockReader 模拟真实传输层：请求 ctx 被取消后 body 读取立即报错。
// 第一行给一条 metadata（证明不是首字节就断），之后一直阻塞到 cancel。
type ctxBlockReader struct {
	ctx  context.Context
	sent bool
}

func (r *ctxBlockReader) Read(p []byte) (int, error) {
	if r.sent {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	r.sent = true
	return copy(p, []byte("event: metadata\ndata: {\"a\":1}\n\n")), nil
}

func TestTraeStreamIdleWatchdog(t *testing.T) {
	t.Setenv(traeIdleEnv, "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &TraeProxy{}
	w := httptest.NewRecorder()
	start := time.Now()
	p.streamToClient(ctx, cancel, w, &ctxBlockReader{ctx: ctx}, "kimi-k3", "rid-idle", time.Now())
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("看门狗没生效：等了 %s", elapsed)
	}
	body := w.Body.String()
	if !strings.Contains(body, "upstream_timeout") {
		t.Errorf("应回 upstream_timeout 错误帧：%q", body)
	}
	if !strings.Contains(body, "没有任何数据") {
		t.Errorf("错误文案要说清是上游静默：%q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("失败路径也要以 [DONE] 收尾，客户端才不会卡在等帧：%q", body)
	}
}

// failWriter 写任何东西都失败，模拟客户端连接已断。
type failWriter struct{ h http.Header }

func (f *failWriter) Header() http.Header         { return f.h }
func (f *failWriter) WriteHeader(int)             {}
func (f *failWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

// countingReader 无限吐 output 帧，记录被读了多少次。
type countingReader struct{ n int }

func (r *countingReader) Read(p []byte) (int, error) {
	r.n++
	if r.n > 500 {
		return 0, io.EOF
	}
	return copy(p, []byte("event: output\ndata: {\"response\":\"x\"}\n\n")), nil
}

func TestTraeStreamStopsWhenClientGone(t *testing.T) {
	t.Setenv(traeIdleEnv, "30") // 别让看门狗先触发，这里专门测写失败路径
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &countingReader{}
	w := &failWriter{h: http.Header{}}
	p := &TraeProxy{}
	done := make(chan struct{})
	go func() {
		p.streamToClient(ctx, cancel, w, body, "kimi-k3", "rid-gone", time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后没有停止读上游")
	}
	if body.n > 20 {
		t.Errorf("客户端走后还读了 %d 行上游数据（应在首帧写失败时就停）", body.n)
	}
}

// 正常流：thinking + content + done，行为不能因这次改动退化。
func TestTraeStreamNormalUnaffected(t *testing.T) {
	t.Setenv(traeIdleEnv, "5")
	stream := strings.Join([]string{
		"event: output",
		`data: {"response":"","reasoning_content":"想一下"}`,
		"",
		"event: output",
		`data: {"response":"收到"}`,
		"",
		"event: done",
		`data: {"finish_reason":"stop"}`,
		"",
	}, "\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &TraeProxy{}
	w := httptest.NewRecorder()
	p.streamToClient(ctx, cancel, w, strings.NewReader(stream), "kimi-k3", "rid-ok", time.Now())
	body := w.Body.String()
	for _, want := range []string{`"reasoning_content":"想一下"`, `"content":"收到"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("正常流缺 %q：%q", want, body)
		}
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("正常流应以 [DONE] 结束：%q", body)
	}
}

// AIHUB_TRAE_EFFORT / trae_effort.txt：客户端没给档位时补默认值。
func TestTraeApplyDefaultEffort(t *testing.T) {
	oldRoot := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = oldRoot })

	t.Setenv(traeEffortEnv, "")
	raw := map[string]any{"model": "kimi-k3"}
	if got := traeApplyDefaultEffort(raw); got != "" {
		t.Fatalf("不设环境变量、没有文件时不应注入，实际 %q", got)
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Fatal("不应写 reasoning_effort")
	}

	// 数据根放文件 → 生效（桌面快捷方式启动时带不了环境变量，这条是主用法）
	if err := os.WriteFile(filepath.Join(hubRoot, traeEffortFile), []byte("LOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw = map[string]any{"model": "kimi-k3"}
	if got := traeApplyDefaultEffort(raw); got != "low" {
		t.Fatalf("文件写 low 应注入 low，实际 %q", got)
	}
	if raw["reasoning_effort"] != "low" {
		t.Fatalf("注入后字段值不对：%v", raw["reasoning_effort"])
	}

	// 环境变量优先于文件
	t.Setenv(traeEffortEnv, "high")
	if got := traeDefaultEffort(); got != "high" {
		t.Fatalf("环境变量应覆盖文件，实际 %q", got)
	}

	// 非法值忽略（不能让上游因为错拼字段值 400）
	if err := os.WriteFile(filepath.Join(hubRoot, traeEffortFile), []byte("ultra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(traeEffortEnv, "")
	if got := traeDefaultEffort(); got != "" {
		t.Fatalf("非法档位应忽略，实际 %q", got)
	}

	// 客户端显式给了档位：尊重客户端，不覆盖
	t.Setenv(traeEffortEnv, "low")
	raw2 := map[string]any{"reasoning_effort": "high"}
	if got := traeApplyDefaultEffort(raw2); got != "" {
		t.Fatalf("客户端已给档位时不应注入，实际 %q", got)
	}
	if raw2["reasoning_effort"] != "high" {
		t.Fatalf("客户端档位被改写了：%v", raw2["reasoning_effort"])
	}
}
