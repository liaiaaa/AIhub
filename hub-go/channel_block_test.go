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
	"time"
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
	for i := 1; i <= channelBlockRetriesMax; i++ {
		cur := channelBlockWait(i).Seconds()
		if cur <= prev {
			t.Fatalf("第 %d 次等待 %.1fs 没有递增（上一个 %.1fs）", i, cur, prev)
		}
		prev = cur
	}
}

// 2026-10-25：默认不重试。反复重试会把一次可疑请求放大成多次，
// 上游 10-23 收紧识别后这是给自己加风险。
func TestChannelBlockRetriesDefaultZero(t *testing.T) {
	t.Setenv(channelBlockRetryEnv, "")
	t.Setenv(channelBlockDisabledEnv, "")
	if got := channelBlockRetries(); got != 0 {
		t.Errorf("默认应为 0 次重试，实际 %d", got)
	}
}

func TestChannelBlockRetriesEnvOverride(t *testing.T) {
	t.Setenv(channelBlockDisabledEnv, "")
	t.Setenv(channelBlockRetryEnv, "2")
	if got := channelBlockRetries(); got != 2 {
		t.Errorf("AIHUB_CHANNEL_RETRY=2 应得到 2，实际 %d", got)
	}
	// 上限保护：不能把重试次数配成无限大
	t.Setenv(channelBlockRetryEnv, "99")
	if got := channelBlockRetries(); got != channelBlockRetriesMax {
		t.Errorf("应截断到上限 %d，实际 %d", channelBlockRetriesMax, got)
	}
	// 非数字/负数回落到默认
	for _, v := range []string{"abc", "-1"} {
		t.Setenv(channelBlockRetryEnv, v)
		if got := channelBlockRetries(); got != 0 {
			t.Errorf("非法值 %q 应回落 0，实际 %d", v, got)
		}
	}
	// 强制关闭开关优先
	t.Setenv(channelBlockRetryEnv, "3")
	t.Setenv(channelBlockDisabledEnv, "1")
	if got := channelBlockRetries(); got != 0 {
		t.Errorf("AIHUB_NO_CHANNEL_RETRY=1 应强制 0，实际 %d", got)
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
	out := string(annotateChannelBlockGiveUp([]byte(chanBlockBody), 3, false, time.Time{}))
	for _, want := range []string{"11128", "重试 3 次", "其它线路", "displayMsg"} {
		if !strings.Contains(out, want) {
			t.Errorf("提示缺 %q：%s", want, out)
		}
	}
	// 默认 0 次重试的文案必须说明「没有静默重试」，避免用户以为 hub 没做事
	out = string(annotateChannelBlockGiveUp([]byte(chanBlockBody), 0, false, time.Time{}))
	if !strings.Contains(out, "不再静默重试") {
		t.Errorf("0 次重试时文案要说明原因：%s", out)
	}
	// 开了闸门时要把截止时刻和「本地闸门」说清楚，不能冒充上游承诺
	out = string(annotateChannelBlockGiveUp([]byte(chanBlockBody), 0, true, time.Now().Add(30*time.Minute)))
	if !strings.Contains(out, "本地闸门") || !strings.Contains(out, "暂停出站到") {
		t.Errorf("闸门文案缺关键信息：%s", out)
	}
	// 非 JSON 体不能崩，且要把说明拼上去
	out = string(annotateChannelBlockGiveUp([]byte("plain text"), 2, false, time.Time{}))
	if !strings.Contains(out, "[hub]") {
		t.Errorf("非 JSON 体应追加说明：%s", out)
	}
}

// 端到端：显式打开 3 次重试后，前 2 次返回 11128、第 3 次成功 ——
// 重试逻辑应吸收掉，调用方最终拿到 200。
func TestRetryChannelBlockAbsorbs(t *testing.T) {
	t.Setenv(channelBlockDisabledEnv, "")
	t.Setenv(channelBlockRetryEnv, "3")
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
	t.Setenv(channelBlockDisabledEnv, "")
	t.Setenv(channelBlockRetryEnv, "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
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
	// 默认 0 次重试：首次 11128 之后不能再打上游
	if calls != 1 {
		t.Errorf("默认不重试时上游只该被调用 1 次，实际 %d", calls)
	}
}
