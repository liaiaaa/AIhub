package main

// Qoder 代理流处理的回归测试。
// 背景：openInferValidated 曾用 bufio.Peek 预读首块后又把 head 拼回流前面，
// 而 Peek 不消费缓冲，导致已缓冲字节被读两遍——客户端 JSON.parse 报
// "Unexpected non-whitespace character after JSON at position N"。
// 本测试让上游一次写完整个 SSE 体（必然整段进 bufio 缓冲），
// 若存在重复/丢失，全量读回必不等于原始字节。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenInferValidatedStreamIntegrity(t *testing.T) {
	upstream := "data: {\"headers\":{},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"Hi\\\"}}]}\"}\n\n" +
		"data: {\"headers\":{},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"!\\\"}}]}\"}\n\n" +
		"data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n" +
		"event:finish\ndata: {}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstream))
	}))
	defer srv.Close()

	p := NewQoderProxy()
	p.inferURL = srv.URL
	p.client = srv.Client()
	a, err := NewQoderAuth() // 只解析内置公钥，不读任何文件
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	p.auth = a

	resp, err := p.openInferValidated(context.Background(), []byte(`{"model":"m","messages":[]}`), nil, "t01", nil)
	if err != nil {
		t.Fatalf("openInferValidated: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != upstream {
		t.Fatalf("流内容被破坏（重复/丢失）:\n got  = %q\n want = %q", got, upstream)
	}
}

// 错误帧触发重试的路径也要保真：首帧是 provider_error 时应重试一次并拿到第二份正常流。
func TestOpenInferValidatedRetriesOnFirstFrameError(t *testing.T) {
	upstream := "data: {\"headers\":{},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"ok\\\"}}]}\"}\n\n" +
		"data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			_, _ = w.Write([]byte("data: {\"code\":\"provider_error\",\"message\":\"insufficient_quota\"}\n\n"))
			return
		}
		_, _ = w.Write([]byte(upstream))
	}))
	defer srv.Close()

	p := NewQoderProxy()
	p.inferURL = srv.URL
	p.client = srv.Client()
	a, err := NewQoderAuth()
	if err != nil {
		t.Fatalf("NewQoderAuth: %v", err)
	}
	a.creds = qoderCreds{Token: "dt-test", User: qoderUser{ID: "u1", Name: "tester"}}
	p.auth = a

	resp, err := p.openInferValidated(context.Background(), []byte(`{}`), nil, "t02", nil)
	if err != nil {
		t.Fatalf("openInferValidated: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != upstream {
		t.Fatalf("重试后流内容被破坏:\n got  = %q\n want = %q", got, upstream)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2（首帧错误应触发一次重试）", calls)
	}
}
