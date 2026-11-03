package main

// TeleAgent 的图片：桌面端其实是**先调视觉模型**，不是把图直接塞给对话模型。
//
// 证据（/opt/TeleAgent 的运行时 bundle，`runtime.js` 里 `/image-read` 那段）：
// 本地起一个小 HTTP 服务，POST /image-read 时把图转成请求转发到
// `${NewApi.baseURL}/chat/completions`（它们自己的视觉聚合口），拿到文字结果再交给主模型。
// 这也解释了为什么 chat-flagship / chat-nano 报「只支持 text」——它们本来就不是多模态。
//
// 所以 aihub 这边同样处理：请求里带图、而目标模型吃不下图时，先把图交给一个
// 视觉模型转成文字描述，再把图片 part 换成文字（并缓存，避免同一张图反复烧钱）。
//
// 2026-11-03 实测背景：直接给 chat-pro 发 image_url 在简单场景能 200，但 DSH 的
// 14 条消息 + 6 张图的请求会被上游 400（`messages.content.type 参数非法，取值范围 ['text']`），
// 而桌面端那条路（视觉模型转文字）不受影响。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	visionCacheMax  = 128 // 图片描述缓存条数上限
	visionTimeout   = 90 * time.Second
	visionMaxPixels = 4096
)

// visionCache 图片哈希 → 文字描述。
var visionCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// visionModelCandidates 依次尝试的视觉模型（第一个能用的胜出）。
// 可用 AIHUB_VISION_MODEL 指定单个模型。
func visionModelCandidates() []string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_VISION_MODEL")); v != "" {
		return []string{v}
	}
	return []string{"qoder.Qwen3.8-Flash", "teleagent.chat-flash", "trae.glm-5.3"}
}

// visionAggURL 聚合网关地址（我们自己，避免绕到外面去）。
func visionAggURL() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_AGG_URL")); v != "" {
		return strings.TrimRight(v, "/") + "/v1/chat/completions"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", AggPort)
}

// describeImage 把一张 data URL 图片交给视觉模型，返回文字描述（带缓存）。
func describeImage(dataURL string) (string, error) {
	sum := sha256.Sum256([]byte(dataURL))
	key := hex.EncodeToString(sum[:])
	visionCache.Lock()
	if v, ok := visionCache.m[key]; ok {
		visionCache.Unlock()
		return v, nil
	}
	visionCache.Unlock()

	var lastErr error
	for _, model := range visionModelCandidates() {
		text, err := askVisionModel(model, dataURL)
		if err != nil {
			lastErr = err
			continue
		}
		visionCache.Lock()
		if len(visionCache.m) >= visionCacheMax {
			visionCache.m = map[string]string{} // 简单粗暴，够用
		}
		visionCache.m[key] = text
		visionCache.Unlock()
		return text, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的视觉模型")
	}
	return "", lastErr
}

// askVisionModel 向指定模型发一次「看图说话」请求。
func askVisionModel(model, dataURL string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 800,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "把这张图里的信息完整转成文字：有文字就逐字抄下来（保持原有顺序与表格结构），没有文字就客观描述画面内容。只输出结果本身，不要客套话。"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			}},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, visionAggURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := directClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("%s 返回无法解析：%w", model, err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("%s：%s", model, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%s 没有返回内容", model)
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("%s 返回空描述", model)
	}
	return text, nil
}

// dataURLBytes 取出 data URL 的原始字节（用于日志里的体积信息）。
func dataURLBytes(u string) []byte {
	_, b64, ok := strings.Cut(u, ",")
	if !ok {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	return b
}

// teleagentVisionEnabled 是否启用「图片转文字」（默认开，AIHUB_TELEAGENT_VISION=0 可关）。
func teleagentVisionEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AIHUB_TELEAGENT_VISION")))
	return v != "0" && v != "off" && v != "false"
}

// replaceImagesWithDescriptions 把请求里所有图片 part 换成视觉模型给出的文字。
// 返回替换张数；一张都没成功时 ok=false（调用方退回去图逻辑）。
func replaceImagesWithDescriptions(payload map[string]any, logf func(string, ...any)) (int, bool) {
	msgs, _ := payload["messages"].([]any)
	replaced := 0
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			t, _ := pm["type"].(string)
			if t != "image_url" {
				out = append(out, p)
				continue
			}
			var u string
			switch v := pm["image_url"].(type) {
			case string:
				u = v
			case map[string]any:
				u, _ = v["url"].(string)
			}
			if !strings.HasPrefix(u, "data:") {
				continue // 非 data URL（附件引用等）没法定，直接丢
			}
			text, err := describeImage(u)
			if err != nil {
				logf("⚠ 图片转文字失败：%v", err)
				continue
			}
			replaced++
			sz := len(dataURLBytes(u))
			logf("图片 %d 字节 → 文字 %d 字", sz, len([]rune(text)))
			out = append(out, map[string]any{
				"type": "text",
				"text": "[图片内容（自动识别）]\n" + text,
			})
		}
		if len(out) == 0 {
			out = append(out, map[string]any{"type": "text", "text": "[图片未能转交]"})
		}
		m["content"] = out
	}
	if replaced == 0 {
		return 0, false
	}
	return replaced, true
}
