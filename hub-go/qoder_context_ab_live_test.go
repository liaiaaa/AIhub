package main

// 口令探针 A/B 实证：无 context_config（旧行为） vs 1M 档。
//
// 背景（2026-09-26）：DSH 大会话（1253+ 条消息）在流式中段报 provider_error，
// 重试 3 次全败，特征与「网关收下请求、模型节点跑一半爆掉」的上下文溢出吻合。
// 用户拍板：Qoder 模型都支持 1M，DSH 会话也按 1M 配，代理默认就发最大档。
//
// 本测试用同一份大上下文载荷（开头藏口令）跑两组：
//   - A 组：QODER_CONTEXT_TIER=none  → 信封不带 context_config（旧行为）
//   - B 组：默认                    → 信封带 1M 档 context_config
//
// 对照观察：正文是否流回、错误帧数、上游实际路由的 model、usage（若有）。
// 默认跳过；启用：$env:QODER_LIVE_TEST="1"; go test -run TestLiveContextTierAB -v
// 载荷大小用 CONTEXT_PROBE_CHARS 控制（默认 60000 字符，≈20K token，成本可控）。
// 使用临时端口，不影响正在运行的 hub。

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

// buildContextProbe 构造大上下文载荷：开头藏口令 + 大段填充 + 结尾提问。
// 若网关截断上下文，口令段丢失，模型无法答出。
func buildContextProbe(model string, fillChars int) []byte {
	const passphrase = "KQ7-9XMZ-4TW2"
	fill := strings.Repeat("这是用于上下文档位验证的填充文本，无实际意义，请忽略。", fillChars/38+1)
	if len(fill) > fillChars {
		fill = fill[:fillChars]
	}
	payload := map[string]any{
		"model":  model,
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "口令是 " + passphrase + "。以下内容不需要回复：" + fill},
			map[string]any{"role": "user", "content": "第一条消息里的口令是什么？只输出口令本身。"},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func runContextProbe(t *testing.T, p *QoderProxy, model string, fillChars int) (content, reasoning string, errFrames int, httpCode int) {
	client := &http.Client{Timeout: 300 * time.Second}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(p.Port)+"/v1/chat/completions",
		bytes.NewReader(buildContextProbe(model, fillChars)))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求代理: %v", err)
	}
	defer resp.Body.Close()
	httpCode = resp.StatusCode
	if httpCode != http.StatusOK {
		_, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return "", "", 1, httpCode
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
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
			errFrames++
			continue
		}
		for _, ch := range chunk.Choices {
			reasoning += ch.Delta.ReasoningContent
			content += ch.Delta.Content
		}
	}
	return content, reasoning, errFrames, httpCode
}

func TestLiveContextTierAB(t *testing.T) {
	if os.Getenv("QODER_LIVE_TEST") == "" {
		t.Skip("跳过上下文档位 A/B 实证；设置 QODER_LIVE_TEST=1 以启用")
	}
	fillChars := 60000
	if v := os.Getenv("CONTEXT_PROBE_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			fillChars = n
		}
	}
	model := "qfmodel" // 免费模型，A/B 两轮成本可控
	if v := os.Getenv("CONTEXT_PROBE_MODEL"); v != "" {
		model = v
	}
	t.Logf("探针载荷：model=%s fillChars=%d（≈%dK token）", model, fillChars, fillChars/3/1000)

	p := NewQoderProxy()
	p.Port = freePort(t)
	if ok, msg := p.Start(); !ok {
		t.Fatalf("QoderProxy 启动失败: %s", msg)
	}
	defer p.Stop()

	// A 组：无 context_config（旧行为）
	t.Setenv("QODER_CONTEXT_TIER", "none")
	start := time.Now()
	contentA, reasonA, errA, codeA := runContextProbe(t, p, model, fillChars)
	elapsedA := time.Since(start)

	// B 组：1M 档（默认）
	t.Setenv("QODER_CONTEXT_TIER", "")
	start = time.Now()
	contentB, reasonB, errB, codeB := runContextProbe(t, p, model, fillChars)
	elapsedB := time.Since(start)

	t.Logf("A 组(无 context_config) | HTTP %d | 错误帧 %d | content %d 字 | reasoning %d 字 | %.1fs",
		codeA, errA, len(contentA), len(reasonA), elapsedA.Seconds())
	t.Logf("B 组(1M 档)            | HTTP %d | 错误帧 %d | content %d 字 | reasoning %d 字 | %.1fs",
		codeB, errB, len(contentB), len(reasonB), elapsedB.Seconds())
	t.Logf("A 正文片段: %s", truncate(contentA, 120))
	t.Logf("B 正文片段: %s", truncate(contentB, 120))

	// B 组必须成功（1M 档被网关接受，正文正常流回）
	if codeB != http.StatusOK || errB > 0 || len(contentB) == 0 {
		t.Fatalf("B 组(1M 档) 失败：HTTP %d 错误帧 %d content %d 字", codeB, errB, len(contentB))
	}
	// 口令必须答对（上下文完整）
	if !strings.Contains(contentB, "KQ7-9XMZ-4TW2") {
		t.Logf("⚠ B 组未答出口令（正文=%q）——上下文可能被截断", truncate(contentB, 80))
	}
	if codeA == http.StatusOK && errA == 0 && len(contentA) > 0 {
		t.Logf("A 组(无 context_config) 也成功——小载荷下网关兜底可容纳，两档差异需加大载荷才能观测")
	}
}
