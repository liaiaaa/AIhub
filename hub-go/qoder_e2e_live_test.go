package main

// 端到端集成测试：在**独立端口**上真启动一个 QoderProxy，验证
//   1) /v1/models 双轨清单正确（28 条 = 14 key + 14 显示名）
//   2) 用「显示名」调用能真实拿到回复（证明别名 → 真实 key 的路由有效）
//   3) 用「真实 key」调用也能拿到回复
//   4) 未知模型名原样透传
//
// 默认跳过；启用：$env:QODER_LIVE_TEST="1"; go test -run TestLiveProxyEndToEnd -v
// 注意：使用临时端口，不会影响正在运行的 hub（8801/8787/8798/8799）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// freePort 取一个空闲端口。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败：%v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestLiveProxyEndToEnd(t *testing.T) {
	if os.Getenv("QODER_LIVE_TEST") == "" {
		t.Skip("跳过端到端集成测试；设置 QODER_LIVE_TEST=1 以启用")
	}

	p := NewQoderProxy()
	p.Port = freePort(t)

	ok, msg := p.Start()
	if !ok {
		t.Fatalf("代理启动失败：%s", msg)
	}
	defer p.Stop()
	t.Logf("代理已在 127.0.0.1:%d 启动：%s", p.Port, msg)

	base := fmt.Sprintf("http://127.0.0.1:%d", p.Port)
	client := &http.Client{Timeout: 120 * time.Second}

	// 等目录刷新完成（Start 里是异步的）
	deadline := time.Now().Add(30 * time.Second)
	var modelIDs map[string]map[string]any
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/v1/models")
		if err == nil {
			var out struct {
				Data []map[string]any `json:"data"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if len(out.Data) > 0 {
				modelIDs = map[string]map[string]any{}
				for _, d := range out.Data {
					if id, _ := d["id"].(string); id != "" {
						modelIDs[id] = d
					}
				}
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if modelIDs == nil {
		t.Fatal("/v1/models 始终为空")
	}

	// 1) 清单：每个模型只一条（显示名做 id），共 14 条且无重复
	t.Logf("/v1/models 返回 %d 条", len(modelIDs))
	if len(modelIDs) != 14 {
		t.Errorf("条目数 = %d, want 14（不许重复）", len(modelIDs))
	}
	for _, want := range []string{
		"Auto", "Qwen3.8-Max", "Qwen3.8-Flash", "Qwen3.7-Max", "Qwen3.7-Plus", "Qwen3.7-Flash",
		"DeepSeek-V4-Pro", "DeepSeek-Flash", "GLM-5.3", "GLM-5.3-Flash", "GLM-5.2",
		"Kimi-K3", "Kimi-K2.8-Preview", "MiniMax-M2.7",
	} {
		if _, ok := modelIDs[want]; !ok {
			t.Errorf("/v1/models 缺少 %q", want)
		}
	}

	// 2)+3) 列表里的显示名与不在列表里的真实 key 都要能真实调用
	cases := []struct{ model, note string }{
		{"GLM-5.3", "列表 id（显示名）→ gmodel"},
		{"DeepSeek-V4-Pro", "列表 id（显示名）→ dmodel"},
		{"Qwen3.8-Flash", "列表 id（显示名）→ qfmodel"},
		{"gmodel", "不在列表里的真实 key 也应可用"},
		{"dmodel", "不在列表里的真实 key 也应可用"},
		{"gfmodel", "不在列表里的真实 key（GLM-5.3-Flash）"},
	}
	for _, c := range cases {
		text, err := chatWithRetry(client, base, c.model, 3)
		if err != nil {
			t.Errorf("[%s] 调用失败：%v", c.model, err)
			continue
		}
		if text == "" {
			t.Errorf("[%s] 连续 3 次都返回空内容", c.model)
			continue
		}
		t.Logf("[%-17s] OK（%s）：%s", c.model, c.note, truncate(text, 60))
	}

	// 4) 未知模型名原样透传（不报错、不改写）
	if _, err := chatWithRetry(client, base, "totally-made-up-model-xyz", 3); err != nil {
		t.Errorf("未知模型名应透传而非报错：%v", err)
	}
}

// chatWithRetry 重试若干次，容忍上游偶发空内容。
//
// 背景（2026-09-23 实测）：Qoder 网关对**所有**模型都会偶发返回空 content
// （直连网关复测 8 次：qfmodel 2/8、gmodel 1/8、kmodel_latest 2/8 为空），
// 这是上游抖动，与本代理无关（改动前的 Qwen3.8-Flash 路径同样如此）。
// 因此集成测试不能对单次调用断言"必须非空"。
func chatWithRetry(client *http.Client, base, model string, attempts int) (string, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		text, err := chatOnce(client, base, model)
		if err != nil {
			lastErr = err
		} else if text != "" {
			return text, nil
		}
		if i < attempts-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", nil
}

// chatOnce 发一次非流式对话，返回回复文本。
func chatOnce(client *http.Client, base, model string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "只回答两个字：收到"}},
		"stream":     false,
		"max_tokens": 32,
	})
	resp, err := client.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析响应失败：%v（原文 %s）", err, truncate(string(body), 200))
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("无 choices：%s", truncate(string(body), 200))
	}
	return out.Choices[0].Message.Content, nil
}

// TestLiveThinkingStreamVisible 验证“可见思考”端到端体验：
// 客户端流式请求 → 代理注入温和思考提示 + 真实 parameters → 模型 <think> 推理
// → 代理转写成 delta.reasoning_content 流回（Cherry Studio 渲染为思考框）。
// 上游有已知抖动（偶发空响应），重试 3 次任一出 reasoning 即可。
func TestLiveThinkingStreamVisible(t *testing.T) {
	if os.Getenv("QODER_LIVE_TEST") == "" {
		t.Skip("跳过可见思考集成测试；设置 QODER_LIVE_TEST=1 以启用")
	}

	p := NewQoderProxy()
	p.Port = freePort(t)
	if ok, msg := p.Start(); !ok {
		t.Fatalf("代理启动失败：%s", msg)
	}
	defer p.Stop()

	base := fmt.Sprintf("http://127.0.0.1:%d", p.Port)
	client := &http.Client{Timeout: 120 * time.Second}

	sawReasoning := false
	for attempt := 1; attempt <= 3; attempt++ {
		payload, _ := json.Marshal(map[string]any{
			"model":    "GLM-5.3",
			"messages": []any{map[string]any{"role": "user", "content": "9.11和9.9哪个大？为什么？"}},
			"stream":   true,
		})
		resp, err := client.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("请求失败：%v", err)
		}
		reasoning, content := "", ""
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
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &chunk) != nil {
				continue
			}
			if len(chunk.Choices) > 0 {
				reasoning += chunk.Choices[0].Delta.ReasoningContent
				content += chunk.Choices[0].Delta.Content
			}
		}
		resp.Body.Close()
		t.Logf("第 %d 次：reasoning=%d 字符 content=%d 字符", attempt, len(reasoning), len(content))
		if len(reasoning) > 0 {
			sawReasoning = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !sawReasoning {
		t.Error("连续 3 次都没有收到 reasoning_content（可见思考失效）")
	}
}
