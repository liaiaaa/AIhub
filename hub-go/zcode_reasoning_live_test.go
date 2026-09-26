package main

// 真机验证：Zcode 代理自动解密桌面端凭据并真实调用 Z.ai。
//
// 默认跳过；启用：$env:ZCODE_LIVE_TEST="1"; go test -run TestLiveZcodeDesktop -v
// 单次小请求，消耗可忽略；使用临时端口，不影响正在运行的 hub。

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

func TestLiveZcodeDesktop(t *testing.T) {
	if os.Getenv("ZCODE_LIVE_TEST") == "" {
		t.Skip("跳过 Zcode 真机验证；设置 ZCODE_LIVE_TEST=1 以启用")
	}
	p := NewZcodeProxy()
	p.Port = freePort(t)
	if ok, msg := p.Start(); !ok {
		t.Fatalf("ZcodeProxy 启动失败: %s", msg)
	}
	defer p.Stop()

	if p.auth == nil {
		t.Fatal("未自动加载桌面端凭据（p.auth == nil）")
	}
	t.Logf("凭据来源: %v", p.auth.Summary())

	payload := `{"model":"glm-5.3","messages":[{"role":"user","content":"13乘17等于多少？只给答案。"}],"stream":true,"reasoning_effort":"low"}`
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
	if content.Len() == 0 {
		t.Fatalf("正文 content 为 0 字——桌面端凭据调用失败")
	}
}
