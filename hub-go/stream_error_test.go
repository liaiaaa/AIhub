package main

// 2026-10-21 开源审查修的 P1：上游 HTTP 200 但在 SSE 流里发错误帧时，
// 此前所有解析器都只认带 choices 的帧，错误被静默跳过 → 客户端拿到
// 「200 + 空内容」，用户归因为「模型不输出」。这里钉住错误帧识别。

import (
	"strings"
	"testing"
)

func TestCollectStreamCapturesErrorFrame(t *testing.T) {
	cases := []struct {
		name string
		sse  string
		want string
	}{
		{
			name: "标准 OpenAI error 帧",
			sse:  "data: {\"error\":{\"message\":\"upstream overloaded\",\"type\":\"server_error\"}}\n\n",
			want: "upstream overloaded",
		},
		{
			name: "网关包装的 msg 形态",
			sse:  "data: {\"error\":{\"code\":6004,\"msg\":\"usage exceeds frequency limit\"}}\n\n",
			want: "usage exceeds frequency limit",
		},
		{
			name: "先有正常帧再报错",
			sse: "data: {\"choices\":[{\"delta\":{\"content\":\"部分\"}}]}\n\n" +
				"data: {\"error\":{\"message\":\"连接中断\"}}\n\n",
			want: "连接中断",
		},
		{
			name: "只有 code 没有 message",
			sse:  "data: {\"error\":{\"code\":11128}}\n\n",
			want: "upstream error code 11128",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := collectStream(strings.NewReader(c.sse))
			if col.errMsg != c.want {
				t.Errorf("错误帧没被捕获：want %q, got %q", c.want, col.errMsg)
			}
		})
	}
}

// 正常流不该被误判成错误。
func TestCollectStreamNormalNotError(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	col := collectStream(strings.NewReader(sse))
	if col.errMsg != "" {
		t.Errorf("正常流被误判成错误：%q", col.errMsg)
	}
	if got := col.build(); got == nil {
		t.Fatal("build 返回 nil")
	}
}
