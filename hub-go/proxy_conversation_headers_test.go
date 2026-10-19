package main

// 上游 2026-10-16 起对 /v2/chat/completions 开「通路校验」：缺会话四头即
// 11128 conversation id missing（流式还回 200 假成功，错误塞在 data:{"error"} 帧里）。
//
// 这组测试钉三件事：
//  1. 四头的形状照抄官方客户端（conversation/request 是 32 位裸 hex，message/request 是 UUID）；
//  2. 置位的信源必须带、没置位的必须**不**带，且不得就地污染调用方传入的 map；
//  3. intl 默认开启、dom 默认关闭（dom 实测不需要，加头等于改坏一个能用的东西）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

type headersAuthStub struct{ h map[string]string }

func (s headersAuthStub) Headers() (map[string]string, error) { return s.h, nil }
func (s headersAuthStub) Summary() (map[string]any, error)    { return nil, nil }

func TestRandConversationIDShapes(t *testing.T) {
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 200; i++ {
		if s := randHex(16); !hexRe.MatchString(s) {
			t.Fatalf("randHex(16) 应为 32 位小写 hex，得到 %q", s)
		}
		if u := randUUID(); !uuidRe.MatchString(u) {
			t.Fatalf("randUUID 不是合法 v4 UUID：%q", u)
		}
	}
}

func TestWithConversationHeadersOnlyWhenEnabled(t *testing.T) {
	in := map[string]string{"Authorization": "Bearer x"}

	p := &Proxy{}
	if out := p.withConversationHeaders(in); len(out) != 1 {
		t.Fatalf("未置位时必须原样返回，得到 %d 个头：%v", len(out), out)
	}

	p.ConversationHeaders = true
	out := p.withConversationHeaders(in)
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID"} {
		if out[k] == "" {
			t.Fatalf("置位后缺头 %s；实际=%v", k, out)
		}
	}
	if out["Authorization"] != "Bearer x" {
		t.Fatal("原有头不能丢")
	}
	if len(in) != 1 {
		t.Fatalf("输入 map 被污染（凭证头是缓存出来的共享值）：%v", in)
	}
	if out["X-Conversation-ID"] == p.withConversationHeaders(in)["X-Conversation-ID"] {
		t.Fatal("每次调用应造新 id：复用同一个会话 id 去重试，等于把上游的重复请求检测留给自己踩")
	}
}

// doUpstream 是唯一出口，头必须在它里面注入 —— 漏掉任何一条请求构造路径，
// 客户端看到的就是随机性失败。
func TestDoUpstreamInjectsHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	auth := headersAuthStub{h: map[string]string{"Authorization": "Bearer tok", "X-User-Id": "u-1"}}
	send := func(p *Proxy) http.Header {
		t.Helper()
		got = nil
		resp, err := p.doUpstream(context.Background(), srv.Client(), auth, srv.URL,
			map[string]string{"Authorization": "Bearer tok", "X-User-Id": "u-1"}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return got
	}

	on := send(&Proxy{ConversationHeaders: true})
	for _, k := range []string{"X-Conversation-Id", "X-Conversation-Request-Id", "X-Conversation-Message-Id", "X-Request-Id"} {
		if on.Get(k) == "" {
			t.Fatalf("置位后上游没收到 %s（头名或注入点不对）：%v", k, on)
		}
	}
	if on.Get("X-User-Id") != "u-1" || on.Get("Authorization") != "Bearer tok" {
		t.Fatalf("基础头被弄丢：%v", on)
	}
	// 两次请求必须是两套 id：复用同一会话 id 等于把上游的重复请求检测留给自己踩。
	again := send(&Proxy{ConversationHeaders: true})
	if again.Get("X-Conversation-Id") == on.Get("X-Conversation-Id") {
		t.Fatal("X-Conversation-ID 每次请求没有重新生成")
	}

	off := send(&Proxy{})
	for _, k := range []string{"X-Conversation-Id", "X-Conversation-Request-Id", "X-Conversation-Message-Id", "X-Request-Id"} {
		if off.Get(k) != "" {
			t.Fatalf("未置位却带了 %s=%q —— 这会把不需要它的信源（dom）请求形状改掉", k, off.Get(k))
		}
	}
	if off.Get("X-User-Id") != "u-1" {
		t.Fatal("关闭态也不能影响原有头")
	}
}

func TestLineFlagsForConversationHeaders(t *testing.T) {
	if !NewIntlProxy(t.TempDir()).ConversationHeaders {
		t.Fatal("intl 必须开 ConversationHeaders：2026-10-16 起不开就是整条线路 11128")
	}
	if NewDomProxy().ConversationHeaders {
		t.Fatal("dom 线路实测不需要会话四头，置位等于改坏一个能正常工作的东西")
	}
}

// 探活与推理是两条独立的请求构造路径。上游的通路校验只认端点不认调用方，
// 所以两条路径都得带会话四头 —— 少一条就是「推理能用、看板全红」的假故障
// （2026-10-16 实际踩过：修完 doUpstream 后三个凭证探活仍 11128）。
func TestProbePathAlsoCarriesConversationHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"delta":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	p := &Proxy{ConversationHeaders: true, UserAgent: "codebuddy-guoji/1.0", Backend: srv.URL}
	headers := p.intlProbeHeaders(map[string]string{"Authorization": "Bearer tok"})
	req, detail, res := intlProbeRequest(p.Backend, headers, intlProbeBody())
	if req == nil {
		t.Fatalf("构造探活请求失败：%s (%v)", detail, res)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for _, k := range []string{"X-Conversation-Id", "X-Conversation-Request-Id", "X-Conversation-Message-Id", "X-Request-Id"} {
		if got.Get(k) == "" {
			t.Fatalf("探活请求没带 %s —— 看板会把好号判成废号", k)
		}
	}
	if got.Get("Authorization") != "Bearer tok" || got.Get("User-Agent") != "codebuddy-guoji/1.0" {
		t.Fatalf("探活基础头被弄坏：%v", got)
	}
	if got.Get("X-Conversation-Id") == "" && p.ConversationHeaders == false {
		t.Fatal("不该走到这里")
	}
	// 关闭态：探活也不该带（dom 线路的探活形状不能被动过）。
	p2 := &Proxy{UserAgent: "x", Backend: srv.URL}
	got = nil
	req2, _, _ := intlProbeRequest(p2.Backend, p2.intlProbeHeaders(map[string]string{"Authorization": "Bearer t"}), intlProbeBody())
	r2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	for _, k := range []string{"X-Conversation-Id", "X-Request-Id"} {
		if got.Get(k) != "" {
			t.Fatalf("未置位的探活却带了 %s", k)
		}
	}
}
