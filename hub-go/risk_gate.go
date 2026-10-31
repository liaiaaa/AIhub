package main

// 冷却闸门：在请求路径上真正拦住「已知被风控 / 限流 / 渠道拦截」的账号。
//
// 背景：rate_limit.json 从 2026-10-12 起就在记 6004 / 11140，但只有控制台在
// 读它——请求照样发给上游，账号被 11140 风控期间还在持续挨打。2026-10-25
// 补上闸门：台账里有账号级（11140）、渠道拦截（11128）或当前模型的限流记录时，
// 本地直接回 429 并且**不调用上游**。
//
// 为什么回 429 而不是 503：客户端（dsh / Codex）对 429 的语义是「限流，可重试」，
// 能据此退避；同时带上 Retry-After 和可读的 hub_hint，人看日志也知道该换线路。

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// gateCooling 检查该账号是否在冷却期内；命中则写好响应并返回 true。
//
// fail-open：取不到 uid、台账读不出来都放行。闸门是为了少打上游，不是把线路
// 锁死——宁可多打一次，也不要因为台账文件坏了让整条线不可用。
func (p *Proxy) gateCooling(w http.ResponseWriter, auth AuthProvider, model, rid string, logger *FileLogger) bool {
	if auth == nil {
		return false
	}
	sum, err := auth.Summary()
	if err != nil {
		return false
	}
	uid, _ := sum["uid"].(string)
	if uid == "" {
		return false
	}
	e, limited := rateLimitFor(uid, model)
	if !limited {
		return false
	}
	wait := time.Until(e.Until)
	if wait < time.Second {
		wait = time.Second
	}
	if logger != nil {
		logger.Log("[%s] ✗ 本地冷却闸门 | %s（%s）| 到 %s",
			rid, e.Reason, e.Kind, rateLimitShortUntil(e.Until))
	}
	w.Header().Set("Retry-After", strconv.Itoa(int((wait+999*time.Millisecond)/time.Second)))
	msg := fmt.Sprintf("账号冷却中：%s；本地闸门到 %s 放行（不是上游承诺的恢复时刻）。"+
		"请改用其它线路/模型，或在控制台换凭证。", e.Reason, rateLimitShortUntil(e.Until))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "rate_limit_error",
			"code":    e.Code,
		},
		"hub_hint": msg,
	})
	return true
}
