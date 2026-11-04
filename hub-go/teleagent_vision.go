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
	// visionPrompt 识图提示词。2026-11-04 用户反馈「旗舰只看到 OCR 结果、内容不够」
	// —— 原来那句「完整转成文字」模型容易偷懒概括，改成明确要求逐行抄录 + 保留结构。
	// 改动提示词会改变缓存 key（见 visionCacheKey），旧缓存自动失效。
	visionPrompt = "请把这张图**逐行完整转写**成文字：" +
		"① 图里所有可见文字都照原文抄下来，不要概括、不要省略、不要改写；" +
		"② 保留原有顺序、标题层级、列表序号、表格行列关系（表格用 Markdown 表格）；" +
		"③ 界面截图还要说明控件位置与状态（按钮文字、输入框占位符、报错文案、时间戳等）；" +
		"④ 如果几乎没有文字，就客观描述画面内容与布局。" +
		"直接输出结果，不要开场白。"
	visionPromptVersion = "v2" // 提示词版本，进缓存 key
	visionMaxTokens     = 3000
	visionCacheMax      = 128 // 图片描述缓存条数上限
	visionTimeout       = 90 * time.Second
	visionMaxPixels     = 4096
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
	// chat-lite 排在第一位：桌面端自己就用它（runtime.js 的 smallModelRef = NewApi/chat-lite），
	// 读截图里的 UI 文案比通用视觉模型更准，而且它是 TeleAgent 自家额度。
	return []string{"teleagent.chat-lite", "qoder.Qwen3.8-Flash", "teleagent.chat-flash"}
}

// visionAggURL 聚合网关地址（我们自己，避免绕到外面去）。
func visionAggURL() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_AGG_URL")); v != "" {
		return strings.TrimRight(v, "/") + "/v1/chat/completions"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", AggPort)
}

// visionCacheKey 图片哈希 + 提示词版本：换提示词后旧描述自动作废。
func visionCacheKey(dataURL string) string {
	sum := sha256.Sum256([]byte(visionPromptVersion + "\x00" + dataURL))
	return hex.EncodeToString(sum[:])
}

// describeImage 把一张 data URL 图片交给视觉模型，返回文字描述（带缓存）。
func describeImage(dataURL string) (string, error) {
	key := visionCacheKey(dataURL)
	visionCache.Lock()
	if v, ok := visionCache.m[key]; ok {
		visionCache.Unlock()
		return v, nil
	}
	visionCache.Unlock()

	var lastErr error
	for _, model := range visionModelCandidates() {
		// 走 TeleAgent 上游直连（而不是绕回自己的聚合口）：
		// 2026-11-03 事故 —— 视觉首选就是 teleagent.chat-lite，绕聚合口会把请求又送回本代理，
		// 于是「转文字」里再触发一次「转文字」，层层嵌套，一次请求拖到 51 秒。
		var text string
		var err error
		if strings.HasPrefix(model, "teleagent.") {
			text, err = askTeleagentVisionDirect(strings.TrimPrefix(model, "teleagent."), dataURL)
		} else {
			text, err = askVisionModel(model, dataURL)
		}
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
		"max_tokens": visionMaxTokens,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": visionPrompt},
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

// teleagentVisionMode 图片处理档位：auto（默认，能吃图的模型透传）/ always（一律转文字）。
// 用 AIHUB_TELEAGENT_VISION=always 切到旧行为。
func teleagentVisionMode() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AIHUB_TELEAGENT_VISION")))
	if v == "always" || v == "force" || v == "1" {
		return "always"
	}
	return "auto"
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

// askTeleagentVisionDirect 直接打 TeleAgent 上游做识图，绕开本机聚合口。
// 这样既避免「转文字 → 聚合口 → 本代理 → 又转文字」的递归，也省一跳。
func askTeleagentVisionDirect(backend, dataURL string) (string, error) {
	tok, err := teleagentToken()
	if err != nil {
		return "", err
	}
	dev, inst, err := teleagentDeviceMeta()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"model":      backend,
		"max_tokens": visionMaxTokens,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": visionPrompt},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			}},
		},
	})
	if err != nil {
		return "", err
	}
	const path = "/superCowork/sapi/api/v1/chat/completions" // 参与签名
	url := teleagentUpstreamBase + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hdrs, err := teleagentSignHeadersAt(tok, dev, inst, http.MethodPost, path, teleagentAppVersion(), body, "", "")
	if err != nil {
		return "", err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
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
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("识图响应无法解析：%w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("识图没有返回内容")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("识图返回空")
	}
	return text, nil
}
