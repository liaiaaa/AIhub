package main

// 2026-10-21：上游国内版也开始报 11128「Illegal API invocation from an
// unapproved channel」。查历史频率（10-10: 13 次、10-13: 6 次、10-21: 1 次）
// 判定为间歇性渠道风控，上游 displayMsg 自己写「请重新发送」，故在 hub 内部
// 静默重试。这组测试钉住「只对渠道类 11128 重试」这个边界。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 真实上游响应（2026-10-21 08:39:40 抓自 converter-cn.log）
const chanBlockBody = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"3dd72df6-b55e-41db-affd-04f2c948863f","displayMsg":{"en":"Request blocked. Please send it again","zh":"请求被拦截，请重新发送"},"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。请用官方版本重新发送。"},"actions":["EDIT_INPUT","SUBMIT_FEEDBACK"]}`

func TestIsChannelBlockBody(t *testing.T) {
	if !isChannelBlockBody(400, []byte(chanBlockBody)) {
		t.Error("真实失败体应判定为渠道拦截")
	}
	// 同码不同 msg：必须区分开，否则会把「重试无解」的问题也拿去重试
	others := []string{
		`{"code":11128,"msg":"conversation id missing"}`,
		`{"code":11128,"msg":"first message is not system prompt"}`,
	}
	for _, b := range others {
		if isChannelBlockBody(400, []byte(b)) {
			t.Errorf("这种 11128 重试无用，不该判定为渠道拦截：%s", b)
		}
	}
	// 别的 400 不能误判（11115 prompt too long 重试多少次都没用）
	if isChannelBlockBody(400, []byte(`{"code":11115,"msg":"prompt is too long"}`)) {
		t.Error("11115 不该被当成渠道拦截")
	}
	// 非 400 一律不碰
	if isChannelBlockBody(429, []byte(chanBlockBody)) {
		t.Error("429 走限流重试，不该被当成渠道拦截")
	}
	if isChannelBlockBody(200, []byte(chanBlockBody)) {
		t.Error("200 不该判定为渠道拦截")
	}
}

func TestChannelBlockWaitEscalates(t *testing.T) {
	prev := 0.0
	for i := 1; i <= channelBlockRetries; i++ {
		cur := channelBlockWait(i).Seconds()
		if cur <= prev {
			t.Fatalf("第 %d 次等待 %.1fs 没有递增（上一个 %.1fs）", i, cur, prev)
		}
		prev = cur
	}
}

func TestChannelBlockDisabledEnv(t *testing.T) {
	t.Setenv(channelBlockDisabledEnv, "1")
	if !channelBlockDisabled() {
		t.Error("设为 1 时应关闭")
	}
	t.Setenv(channelBlockDisabledEnv, "")
	if channelBlockDisabled() {
		t.Error("空值应视为开启重试")
	}
}

// annotateChannelBlockGiveUp 必须保留上游原有字段并追加可执行建议
func TestAnnotateChannelBlockGiveUp(t *testing.T) {
	out := string(annotateChannelBlockGiveUp([]byte(chanBlockBody), 3))
	for _, want := range []string{"11128", "重试 3 次", "换一条线路", "displayMsg"} {
		if !strings.Contains(out, want) {
			t.Errorf("提示缺 %q：%s", want, out)
		}
	}
	// 非 JSON 体不能崩，且要把说明拼上去
	out = string(annotateChannelBlockGiveUp([]byte("plain text"), 2))
	if !strings.Contains(out, "[hub]") {
		t.Errorf("非 JSON 体应追加说明：%s", out)
	}
}

// 端到端：前 2 次返回 11128、第 3 次成功 —— 重试逻辑应吸收掉，
// 调用方最终拿到 200。
func TestRetryChannelBlockAbsorbs(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, chanBlockBody)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"OK"}}]}`)
	}))
	defer srv.Close()

	p := &Proxy{}
	// 先手动发一次拿到 400，再交给重试函数
	first, err := p.doUpstream(t.Context(), srv.Client(), nil, srv.URL, nil, []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("首次请求失败: %v", err)
	}
	if first.StatusCode != http.StatusBadRequest {
		t.Fatalf("首次应是 400，实际 %d", first.StatusCode)
	}
	resp, err := p.retryChannelBlock(t.Context(), srv.Client(), nil, srv.URL, nil,
		[]byte(`{"model":"x"}`), nil, "rid-test", first, nil)
	if err != nil {
		t.Fatalf("重试过程报错: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("重试后应 200，实际 %d：%s", resp.StatusCode, body)
	}
	if calls != 3 {
		t.Errorf("应共请求 3 次（1 次原始 + 2 次重试），实际 %d", calls)
	}
}

// 重试全失败时：保留 400 并附上 hub 提示，不能变成 200 或空响应。
func TestRetryChannelBlockGivesUpWithHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, chanBlockBody)
	}))
	defer srv.Close()

	p := &Proxy{}
	first, _ := p.doUpstream(t.Context(), srv.Client(), nil, srv.URL, nil, []byte(`{}`))
	resp, err := p.retryChannelBlock(t.Context(), srv.Client(), nil, srv.URL, nil,
		[]byte(`{}`), nil, "rid-giveup", first, nil)
	if err != nil {
		t.Fatalf("应返回响应而不是错误: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("放弃重试后应仍是 400，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "渠道校验拦截（11128）") {
		t.Errorf("放弃时要带 hub 提示：%s", body)
	}
}
