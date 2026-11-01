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

// trae 的思考档位：客户端 > 手动档位(env/文件) > 该模型最高档。
//
// 2026-11-01 改口径：官方档位名是 light / high / extra_high（来自 Trae 客户端缓存的
// reasoning_effort_config.options），与 dom/intl/qoder/zcode 的「默认拉满」对齐 ——
// 客户端不指定时注入该模型支持的最高档。
func TestTraeApplyDefaultEffort(t *testing.T) {
	oldRoot := hubRoot
	hubRoot = t.TempDir()
	t.Cleanup(func() { hubRoot = oldRoot })
	effortFile := filepath.Join(hubRoot, traeEffortFile)

	t.Setenv(traeEffortEnv, "")

	// ① 什么都不设：自动取该模型最高档
	for _, c := range []struct{ model, want string }{
		{"kimi-k3", "extra_high"},
		{"deepseek-v4.1-flash", "extra_high"}, // 官方默认是 high，我们要更高一档
		{"glm-5.3", "extra_high"},
		{"Doubao-Seed-2.1-Turbo", "high"}, // 只有两档
		{"step-5-preview", "high"},
	} {
		raw := map[string]any{"model": c.model}
		if got := traeApplyDefaultEffort(raw, c.model); got != c.want {
			t.Errorf("%s 默认档位应为 %q，实际 %q", c.model, c.want, got)
		}
		if raw["reasoning_effort"] != c.want {
			t.Errorf("%s 注入后字段值不对：%v", c.model, raw["reasoning_effort"])
		}
	}

	// ② 不支持思考的模型：绝不注入
	for _, m := range []string{"mimo-v2.6-flash", "mimo-v2.6-pro", "minimax-m3", "Doubao-Seed-Evolving"} {
		raw := map[string]any{"model": m}
		if got := traeApplyDefaultEffort(raw, m); got != "" {
			t.Errorf("%s 官方 support_thinking=false，不该注入，实际 %q", m, got)
		}
		if _, ok := raw["reasoning_effort"]; ok {
			t.Errorf("%s 不该写 reasoning_effort", m)
		}
	}

	// ③ 上游目录里已经没有的老模型：不知道它认哪些档位名 → 不猜
	raw := map[string]any{"model": "glm-5.2"}
	if got := traeApplyDefaultEffort(raw, "glm-5.2"); got != "" {
		t.Errorf("未知老模型不该自动注入，实际 %q", got)
	}

	// ④ 数据根放文件：生效，且旧词 low 要归一化成 light
	if err := os.WriteFile(effortFile, []byte("LOW\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw = map[string]any{"model": "kimi-k3"}
	if got := traeApplyDefaultEffort(raw, "kimi-k3"); got != "light" {
		t.Errorf("文件写 LOW 应归一化成 light，实际 %q", got)
	}

	// ⑤ 环境变量优先于文件
	t.Setenv(traeEffortEnv, "high")
	if got := traeManualEffort(); got != "high" {
		t.Errorf("环境变量应覆盖文件，实际 %q", got)
	}

	// ⑥ 非法值忽略
	if err := os.WriteFile(effortFile, []byte("ultra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(traeEffortEnv, "")
	if got := traeManualEffort(); got != "" {
		t.Errorf("非法档位应忽略，实际 %q", got)
	}

	// ⑦ 手动档位该模型不支持 → 不硬塞
	t.Setenv(traeEffortEnv, "extra_high")
	raw = map[string]any{"model": "Doubao-Seed-2.1-Turbo"} // 只有 light/high
	if got := traeApplyDefaultEffort(raw, "Doubao-Seed-2.1-Turbo"); got != "" {
		t.Errorf("该模型没有 extra_high 档，不该注入，实际 %q", got)
	}

	// ⑧ 客户端显式给了档位：尊重客户端，一律不覆盖（哪怕是非法值也不改）
	t.Setenv(traeEffortEnv, "light")
	raw2 := map[string]any{"reasoning_effort": "extra_high"}
	if got := traeApplyDefaultEffort(raw2, "deepseek-v4.1-flash"); got != "" {
		t.Errorf("客户端已给档位时不应注入，实际 %q", got)
	}
	if raw2["reasoning_effort"] != "extra_high" {
		t.Errorf("客户端档位被改写了：%v", raw2["reasoning_effort"])
	}
}
