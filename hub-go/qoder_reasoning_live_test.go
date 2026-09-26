package main

// 真机验证：qodercli 信封改造后，原生 reasoning_content 必须流回。
//
// 背景（2026-09-25）：信封对齐前日志常年 think=0帧；改造后本测试用真实桌面端
// 凭据发一个小请求，统计流里的 reasoning_content 帧。
//
// 默认跳过；启用：$env:QODER_LIVE_TEST="1"; go test -run TestLiveNativeReasoningStream -v
// 使用临时端口，不影响正在运行的 hub。单次小请求，消耗可忽略。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveNativeReasoningStream(t *testing.T) {
	if os.Getenv("QODER_LIVE_TEST") == "" {
		t.Skip("跳过原生推理真机验证；设置 QODER_LIVE_TEST=1 以启用")
	}
	p := NewQoderProxy()
	p.Port = freePort(t)
	if ok, msg := p.Start(); !ok {
		t.Fatalf("QoderProxy 启动失败: %s", msg)
	}
	defer p.Stop()

	payload := `{"model":"gmodel","messages":[{"role":"user","content":"13乘17等于多少？只给答案。"}],"stream":true}`
	req, err := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(p.Port)+"/v1/chat/completions",
		bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求代理: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		t.Fatalf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 500))
	}

	reasoning := &strings.Builder{}
	content := &strings.Builder{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
					Content          string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &chunk) != nil {
			continue
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			t.Fatalf("流内错误帧: %s", chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			reasoning.WriteString(ch.Delta.ReasoningContent)
			content.WriteString(ch.Delta.Content)
		}
	}

	t.Logf("reasoning_content %d 字 | content %d 字", reasoning.Len(), content.Len())
	t.Logf("思考片段(前300字): %s", truncate(reasoning.String(), 300))
	t.Logf("正文片段(前200字): %s", truncate(content.String(), 200))
	if reasoning.Len() == 0 {
		t.Fatalf("原生 reasoning_content 为 0 字——信封改造未生效，思考仍未流回")
	}
	if content.Len() == 0 {
		t.Fatalf("正文 content 为 0 字——上游返回异常")
	}
}
