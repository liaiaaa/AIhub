package main

// 诊断:重放 DSH 真实会话载荷,复现 provider_error(配合二分定位)。
// 用法：$env:QODER_LIVE_TEST="1"; $env:REPLAY_FILE="<载荷.json>"; go test -run TestLiveQoderReplay -v
// 现在同时输出流内的 model 与 usage（核实网关实际路由与接受的 token 数）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveQoderReplay(t *testing.T) {
	if os.Getenv("QODER_LIVE_TEST") == "" {
		t.Skip("diagnostic")
	}
	path := os.Getenv("REPLAY_FILE")
	if path == "" {
		t.Skip("REPLAY_FILE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	t.Logf("payload %d KB", len(raw)/1024)

	p := NewQoderProxy()
	p.Port = freePort(t)
	if ok, msg := p.Start(); !ok {
		t.Fatalf("start: %s", msg)
	}
	defer p.Stop()

	client := &http.Client{Timeout: 300 * time.Second}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(p.Port)+"/v1/chat/completions", bytes.NewReader(raw))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	t.Logf("HTTP %d", resp.StatusCode)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		nContent, nReasoning, nErr := 0, 0, 0
		var lastErr, model string
		var usage map[string]any
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") || line == "data: [DONE]" {
				continue
			}
			rawLine := strings.TrimSpace(line[5:])
			var probe struct {
				Model   string `json:"model"`
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage map[string]any `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Headers map[string]any `json:"headers"`
				Body    string         `json:"body"`
			}
			if json.Unmarshal([]byte(rawLine), &probe) != nil {
				continue
			}
			if probe.Model != "" {
				model = probe.Model
			}
			if probe.Usage != nil {
				usage = probe.Usage
			}
			// 错误帧：裸帧（code/message）或包装帧（headers/body）都打印完整原文
			if (probe.Error != nil && probe.Error.Message != "") || probe.Code != "" || (probe.Body != "" && probe.Choices == nil) {
				nErr++
				lastErr = probe.Message
				if lastErr == "" && probe.Error != nil {
					lastErr = probe.Error.Message
				}
				if probe.Code != "" {
					lastErr = fmt.Sprintf("%s: %s", probe.Code, lastErr)
				}
				t.Logf("⚠ 原始错误帧全文: %s", rawLine)
			}
			for _, ch := range probe.Choices {
				nContent += len(ch.Delta.Content)
				nReasoning += len(ch.Delta.ReasoningContent)
			}
		}
		t.Logf("stream: content %d 字 | reasoning %d 字 | 错误帧 %d | lastErr: %s", nContent, nReasoning, nErr, truncate(lastErr, 300))
		if model != "" {
			t.Logf("上游实际路由 model: %s", model)
		}
		if len(usage) > 0 {
			u, _ := json.Marshal(usage)
			t.Logf("usage: %s", u)
		} else {
			t.Logf("usage: 流内未返回（网关不发布单请求 token 明细）")
		}
		if nErr > 0 {
			t.Fail()
		}
	} else {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		t.Logf("body: %s", truncate(string(b), 400))
	}
}
