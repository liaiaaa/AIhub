package main

// 网关排队帧（业务码 10605）识别与队列感知重试（2026-09-26 晚高峰事故）。
//
// 现象：Qoder 网关在高峰期对请求返回 HTTP 200 + 业务错误帧，外层 code=403、
// message 里再嵌两层 JSON（观测于会话 session-446ef8d8，qfmodel）：
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,
//	 \\\"modelKey\\\":\\\"qfmodel\\\",\\\"queueCount\\\":8445,\\\"queueType\\\":\\\"p3\\\",
//	 \\\"retryAfterSeconds\\\":30,\\\"serviceAvailable\\\":true,\\\"waitTime\\\":261}\"}"}
//
// 旧路径把它当普通首帧错误只重试 2 次×700ms，永远等不过 30s 的建议间隔；
// 随后帧被转写成「上游网关错误 403: …」透传，DSH 客户端按 403 分类显示
// 「API 密钥无效」——凭据其实完全正常，纯粹是网关排队。
//
// 处理：识别排队帧后按 retryAfterSeconds 真实等待重试（总预算内）；
// 预算耗尽仍排队时以 429 语义报「上游排队中」，文案刻意不含 403 字样。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// qoderQueueInfo 排队帧携带的队列状态（字段取最深一层携带者）。
type qoderQueueInfo struct {
	Code              string // 命中的业务码（观测 "10605"）
	QueueCount        int    // 当前排队人数
	QueueType         string // 队列优先级（观测 "p3"）
	WaitTime          int    // 网关预估等待秒数
	RetryAfterSeconds int    // 网关建议的重试间隔秒数
	ServiceAvailable  bool   // 网关是否仍在服务
	hasServiceFlag    bool   // 帧里是否明示 serviceAvailable（缺省视为可用）
}

// serviceUp 明示 false 才判定服务不可用；帧里没带该字段时按可用处理。
func (qi qoderQueueInfo) serviceUp() bool { return !qi.hasServiceFlag || qi.ServiceAvailable }

// qoderQueueWaitEnv 队列重试总预算（秒）；设 0 关闭排队等待（立即报 429）。
const qoderQueueWaitEnv = "QODER_QUEUE_MAX_WAIT_SECS"

// qoderQueueWaitDefault 默认总预算：观测 waitTime 在 115~261s 之间波动，
// 240s 内按网关建议间隔（30s）可重试约 8 次，多数排队能清掉。
const qoderQueueWaitDefault = 240

func qoderQueueWaitBudget() time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(qoderQueueWaitEnv))); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	return qoderQueueWaitDefault * time.Second
}

// parseQueueFrame 从任意帧 JSON 文本中识别排队帧。帧可能多层嵌套
// （SSE 信封 body → 外层 code=403 → message 内嵌 10605 → 再内嵌 isQueued
// 详情），逐层下钻，队列字段取最深一层携带者。非排队帧返回 ok=false。
func parseQueueFrame(raw string) (qoderQueueInfo, bool) {
	s := strings.TrimSpace(raw)
	var qi qoderQueueInfo
	found := false
	for depth := 0; depth < 4; depth++ {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) != nil {
			break
		}
		if b, ok := m["body"].(string); ok && strings.TrimSpace(b) != "" {
			s = strings.TrimSpace(b) // SSE 信封包装帧：剥一层
			continue
		}
		if c, _ := m["code"].(string); c != "" {
			qi.Code = c
			if c == "10605" {
				found = true
			}
		}
		if v, _ := m["isQueued"].(bool); v {
			found = true
			if qi.Code == "" {
				qi.Code = "10605"
			}
		}
		if n, ok := jsonFrameInt(m, "queueCount"); ok {
			qi.QueueCount = n
		}
		if n, ok := jsonFrameInt(m, "waitTime"); ok {
			qi.WaitTime = n
		}
		if n, ok := jsonFrameInt(m, "retryAfterSeconds"); ok {
			qi.RetryAfterSeconds = n
		}
		if qt, _ := m["queueType"].(string); qt != "" {
			qi.QueueType = qt
		}
		if sa, ok := m["serviceAvailable"].(bool); ok {
			qi.ServiceAvailable = sa
			qi.hasServiceFlag = true
		}
		msg, ok := m["message"].(string)
		if !ok || !strings.HasPrefix(strings.TrimSpace(msg), "{") {
			break
		}
		s = strings.TrimSpace(msg)
	}
	return qi, found
}

func jsonFrameInt(m map[string]any, key string) (int, bool) {
	if f, ok := m[key].(float64); ok {
		return int(f), true
	}
	return 0, false
}

// firstFrameQueueInfo 在流的前几个字节里找首个决定性帧并判定是否排队帧。
func firstFrameQueueInfo(head []byte) (qoderQueueInfo, bool) {
	data, ok := decisiveFirstFrame(head)
	if !ok {
		return qoderQueueInfo{}, false
	}
	return parseQueueFrame(data)
}

// qoderQueueNextWait 计算本次排队重试的等待时长；giveup=true 表示应当放弃
// （网关明示不可用 / 预算关闭 / 本次等待将超出总预算）。
func qoderQueueNextWait(ctx context.Context, qi qoderQueueInfo, waited, budget time.Duration) (time.Duration, bool) {
	if !qi.serviceUp() {
		return 0, true
	}
	if budget <= 0 {
		return 0, true
	}
	d := time.Duration(qi.RetryAfterSeconds) * time.Second
	if d <= 0 {
		d = 30 * time.Second // 网关观测建议值
	}
	if d > time.Minute {
		d = time.Minute
	}
	if waited+d > budget {
		return 0, true
	}
	return d, false
}

// qoderSleepCtx 可被取消的等待（客户端断开/聚合超时立即返回，不再空等）。
func qoderSleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// qoderQueueError 排队放弃错误（429 语义）。文案不含 403/密钥字样，
// 避免客户端误判成鉴权失败。
type qoderQueueError struct {
	Info   qoderQueueInfo
	Waited time.Duration // 放弃前已累计等待的时长
}

func (e *qoderQueueError) Error() string {
	if !e.Info.serviceUp() {
		return fmt.Sprintf("上游服务暂不可用（429）：Qoder 网关排队帧 serviceAvailable=false，请稍后重试")
	}
	queueDesc := ""
	if e.Info.QueueCount > 0 {
		queueDesc = fmt.Sprintf("队列 %d 人", e.Info.QueueCount)
		if e.Info.QueueType != "" {
			queueDesc += "（" + e.Info.QueueType + "）"
		}
		queueDesc += "，"
	}
	switch {
	case e.Waited > 0:
		return fmt.Sprintf("上游排队中（429）：Qoder 网关%s已自动等待 %d 秒仍未入队，请稍后重试",
			queueDesc, int(e.Waited.Seconds()+0.5))
	case e.Info.WaitTime > 0:
		return fmt.Sprintf("上游排队中（429）：Qoder 网关%s预估等待 %d 秒，请稍后重试",
			queueDesc, e.Info.WaitTime)
	default:
		return fmt.Sprintf("上游排队中（429）：Qoder 网关%s请稍后重试", queueDesc)
	}
}

// HTTPStatus 让调用方以 429 而非 502 语义回给客户端。
func (e *qoderQueueError) HTTPStatus() int { return http.StatusTooManyRequests }
