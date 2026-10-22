package main

// 2026-10-21 开源审查修的 P2：usageTap 原先只在单次 Write 的 chunk 内做
// LastIndex("usage") + 括号配对。SSE 帧跨 32KB 边界时配对必然失败，usage
// 被静默丢掉（用量统计少记）。这里钉住跨块场景。

import (
	"net/http/httptest"
	"testing"
)

func newTap() (*usageTap, *httptest.ResponseRecorder) {
	rr := httptest.NewRecorder()
	return &usageTap{w: rr}, rr
}

func TestUsageTapSameChunk(t *testing.T) {
	tap, _ := newTap()
	tap.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	if tap.usage == nil {
		t.Fatal("单块内的 usage 没被取到")
	}
	if got := tap.usage["prompt_tokens"]; got == nil {
		t.Errorf("usage 内容不对：%v", tap.usage)
	}
}

func TestUsageTapAcrossChunks(t *testing.T) {
	tap, _ := newTap()
	// 把 usage 对象切在两块中间 —— 单块内永远配不出完整对象
	tap.Write([]byte(`data: {"choices":[],"usa`))
	tap.Write([]byte(`ge":{"prompt_tokens":42,"completion_tokens":7}}`))
	if tap.usage == nil {
		t.Fatal("跨块的 usage 没被取到（回归：只在单 chunk 内匹配）")
	}
	if v, ok := tap.usage["prompt_tokens"]; !ok || v == nil {
		t.Errorf("跨块 usage 内容不对：%v", tap.usage)
	}
}

// 正文里出现 "usage" 这个词、但后面不是对象时，不该配出垃圾。
func TestUsageTapWordInContentNoFalsePositive(t *testing.T) {
	tap, _ := newTap()
	tap.Write([]byte(`data: {"choices":[{"delta":{"content":"check your usage stats"}}]}`))
	if tap.usage != nil {
		t.Errorf("正文里的 usage 一词被误判成配额对象：%v", tap.usage)
	}
}

// 尾部缓冲不能无限增长。
func TestUsageTapTailBounded(t *testing.T) {
	tap, _ := newTap()
	big := make([]byte, 100<<10)
	for i := range big {
		big[i] = 'x'
	}
	tap.Write(big)
	if len(tap.tail) > usageTailKeep {
		t.Errorf("tail 缓冲失控：%d 字节 > 上限 %d", len(tap.tail), usageTailKeep)
	}
}
