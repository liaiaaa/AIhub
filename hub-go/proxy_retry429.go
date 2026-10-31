package main

// 429（限流）感知重试：在 hub-go 内部静默吸收瞬时限流。
//
// 背景（2026-10-12 用户报）：Codex 经聚合口用 intl.deepseek-v4.1-flash 时
// 用着用着报 "exceeded retry limit, last status: 429 Too Many Requests"。
// 查 converter-intl.log 发现上游有两种 429，性质完全不同：
//
//	code=14003  "too many requests" + retry_policy:{"retry":true}
//	            → 瞬时限流，网关自己明示「可以重试」
//	code=6004   "usage exceeds frequency limit ... reset at 2026-10-12 20:01:25 UTC+8"
//	            → 账号级配额耗尽，要等到指定时刻，短退避毫无意义
//
// 而 hub-go 收到 429 是【原样透传】给客户端的（既不读 retry_policy，也不读
// Retry-After）。客户端于是立刻重试，又撞在同一个限流窗口上，几次之后
// 就报 exceeded retry limit —— 上游说的「稍后重试」和客户端「马上重试」
// 节奏没对上，必然失败。
//
// 处理：可重试的 429 在 hub-go 内部按退避等待后重试（总预算内），客户端
// 基本无感；配额耗尽型不重试，改为把「几点恢复」写清楚交给客户端。
// 思路与 Qoder 的排队重试（qoder_queue.go）一致，只是这里针对标准 429。
//
// 与 qoder_queue.go 的区别：那条路是 HTTP 200 + 业务错误帧（网关包装过），
// 这条是标准 HTTP 429，两条互不干扰。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// retry429BudgetEnv 退避重试总预算（秒）。设 0 关闭（退回原样透传 429）。
const retry429BudgetEnv = "HUB_429_MAX_WAIT_SECS"

// retry429BudgetDefault 默认总预算。按 1/2/4/8/16/20s 退避可在 60s 内重试
// 约 6 次；实测 14003 多为秒级~十几秒的瞬时窗口，这个预算足够覆盖。
const retry429BudgetDefault = 60

// retry429MaxSingleWait 单次等待上限，避免 Retry-After 给个超大值把预算一次吃光。
const retry429MaxSingleWait = 20 * time.Second

// retry429BodyLimit 读 429 响应体的上限（只为解析提示文案）。
const retry429BodyLimit = 64 << 10

func retry429Budget() time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(retry429BudgetEnv))); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	return retry429BudgetDefault * time.Second
}

// resetAtRe 抓 6004 文案里的恢复时刻，例：
// "your usage will reset at 2026-10-12 20:01:25 UTC+8"
var resetAtRe = regexp.MustCompile(`reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\s*UTC([+-]\d{1,2})`)

// retry429Plan 一次 429 的处理方案。
type retry429Plan struct {
	retryable bool
	wait      time.Duration
	// hint 不可重试时给用户的说明（会并入错误体，让客户端看到人话）
	hint string
}

// planRetry429 决定这次 429 该不该重试、等多久。
func planRetry429(header http.Header, body []byte, attempt int) retry429Plan {
	// 1) Retry-After 头最权威（HTTP 标准）。不在这里砍上限——上游说等 30s
	//    就该等 30s；若超出总预算，由调用方的预算检查统一交还给客户端。
	if ra := strings.TrimSpace(header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return retry429Plan{retryable: true, wait: time.Duration(secs) * time.Second}
		}
	}

	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		msg, _ := m["msg"].(string)

		// 2) 配额耗尽型：等到指定时刻，短退避无意义 → 不重试，但把时间写清楚
		if strings.Contains(msg, "frequency limit") {
			if resetAt, ok := parseResetAt(msg); ok {
				d := time.Until(resetAt)
				if d <= 0 {
					// 恢复时刻已过，按瞬时限流处理
					return retry429Plan{retryable: true, wait: backoff429(attempt)}
				}
				return retry429Plan{
					retryable: false,
					hint: fmt.Sprintf("\n\n[hub] 这是账号级用量配额耗尽（不是内容问题），上游称 %s 恢复。"+
						"等待期内重试无效；可到控制台改用其它线路/模型，或点「换凭证并重启」换账号。",
						resetAt.Format("2006-01-02 15:04:05")),
				}
			}
			// 有 frequency limit 字样但没解析出时刻：保守起见不重试
			return retry429Plan{retryable: false, hint: quotaHintZh}
		}

		// 3) 网关明示可重试（14003 的 retry_policy.retry）
		if rp, ok := m["retry_policy"].(map[string]any); ok {
			if b, ok := rp["retry"].(bool); ok && b {
				return retry429Plan{retryable: true, wait: backoff429(attempt)}
			}
		}
	}

	// 4) 裸 429 / 无法识别：按瞬时限流保守处理（标准 429 语义就是「稍后重试」）
	return retry429Plan{retryable: true, wait: backoff429(attempt)}
}

const quotaHintZh = "\n\n[hub] 这是账号级用量配额限制（不是内容问题）：等待期内重试无效，" +
	"可到控制台改用其它线路/模型，或点「换凭证并重启」换账号。"

// parseResetAt 从文案里解析恢复时刻（带 UTC±N 偏移）。
func parseResetAt(msg string) (time.Time, bool) {
	mm := resetAtRe.FindStringSubmatch(msg)
	if mm == nil {
		return time.Time{}, false
	}
	off, err := strconv.Atoi(mm[2])
	if err != nil {
		return time.Time{}, false
	}
	zone := time.FixedZone("", off*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", mm[1], zone)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// backoff429 指数退避 + 抖动：1s, 2s, 4s, 8s, 16s, 然后 20s 封顶。
// 抖动避免多个会话同一时刻齐步重试、再次撞在一起。
func backoff429(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := time.Second << uint(min(attempt-1, 5)) // 1,2,4,8,16,32
	if base > retry429MaxSingleWait {
		base = retry429MaxSingleWait
	}
	jitter := time.Duration(mrand.Int63n(int64(base/2))) - base/4 // ±25%
	w := base + jitter
	if w < 200*time.Millisecond {
		w = 200 * time.Millisecond
	}
	return w
}

// sleepCtx 等待 d；ctx 被取消（客户端断开）时提前返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// doUpstreamRetry429 包住 doUpstream：遇到可重试的 429 就在预算内退避重试。
// 预算耗尽 / 不可重试时，把「已重试 N 次」写进错误体再交出去，
// 让客户端（和用户）看到人话而不是干巴巴的 429。
func (p *Proxy) doUpstreamRetry429(ctx context.Context, client *http.Client, auth AuthProvider, url string, headers map[string]string, payload []byte, logger *FileLogger, rid string) (*http.Response, error) {
	resp, err := p.doUpstream(ctx, client, auth, url, headers, payload)
	if err != nil {
		return resp, err
	}
	// 渠道校验拦截（11128）优先于 429 处理：它是随机拦截、等待短，
	// 而且和限流是两回事（见本文件末尾 channelBlock 段注释）。
	resp, err = p.retryChannelBlock(ctx, client, auth, url, headers, payload, logger, rid, resp, err)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	budget := retry429Budget()
	if budget <= 0 {
		return resp, nil // 显式关闭：退回原样透传
	}

	var waited time.Duration
	for attempt := 1; ; attempt++ {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, retry429BodyLimit))
		resp.Body.Close()
		hdr := resp.Header

		plan := planRetry429(hdr, body, attempt)
		if !plan.retryable {
			logger.Log("[%s] ✗ 429 不可重试（配额类）| %s", rid, truncate(string(body), 160))
			recordRateLimitFromBody(auth, payload, body)
			return synthResponse(http.StatusTooManyRequests, hdr, annotateRetryGiveUp(body, plan.hint, attempt-1, waited)), nil
		}
		if waited+plan.wait > budget {
			logger.Log("[%s] ✗ 429 退避预算耗尽（重试 %d 次，共等 %.1fs）", rid, attempt-1, waited.Seconds())
			return synthResponse(http.StatusTooManyRequests, hdr, annotateRetryGiveUp(body, "", attempt-1, waited)), nil
		}

		logger.Log("[%s] ⏳ 429 退避 %.1fs 后重试（第 %d 次，已等 %.1fs/预算 %.0fs）",
			rid, plan.wait.Seconds(), attempt, waited.Seconds(), budget.Seconds())
		if !sleepCtx(ctx, plan.wait) {
			// 客户端已断开：没必要再打上游
			logger.Log("[%s] ✗ 429 退避中被取消", rid)
			return synthResponse(http.StatusTooManyRequests, hdr, annotateRetryGiveUp(body, "", attempt-1, waited)), nil
		}
		waited += plan.wait

		resp, err = p.doUpstream(ctx, client, auth, url, headers, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			if attempt > 1 {
				logger.Log("[%s] ✓ 429 退避重试成功（第 %d 次，共等 %.1fs）", rid, attempt, waited.Seconds())
			}
			return resp, nil
		}
	}
}

// annotateRetryGiveUp 给放弃重试的 429 补一句人话。
// 优先并入上游的 displayMsg（与 annotateRiskControl 同一套做法），
// 没有 displayMsg 时退到顶层 hub_hint 字段。
func annotateRetryGiveUp(body []byte, extra string, attempts int, waited time.Duration) []byte {
	note := extra
	if note == "" && attempts > 0 {
		note = fmt.Sprintf("\n\n[hub] 上游限流：已在 %.0f 秒内退避重试 %d 次仍未通过。"+
			"这不是内容问题，稍等片刻重发即可；持续出现可在控制台换线路/模型。",
			waited.Seconds(), attempts)
	}
	if note == "" {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return append(body, []byte(note)...)
	}
	if dm, ok := m["displayMsg"].(map[string]any); ok {
		if zh, ok := dm["zh"].(string); ok {
			dm["zh"] = zh + note
		} else {
			dm["zh"] = strings.TrimPrefix(note, "\n\n")
		}
		if en, ok := dm["en"].(string); ok {
			dm["en"] = en + retry429HintEn
		}
	} else {
		m["hub_hint"] = strings.TrimPrefix(note, "\n\n")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

const retry429HintEn = "\n\n[hub] Upstream rate limit (not a content problem). " +
	"Retrying within the window did not help; wait a moment and resend, or switch line/model in the hub console."

// synthResponse 用已读出的 body 造一个可再读的响应。
//
// 注意：body 可能被 annotateRetryGiveUp 加长过，所以必须**重算 Content-Length**
// 并剔除传输层头（Content-Length / Transfer-Encoding / Content-Encoding），
// 否则 header 与实际 body 不符——将来若有调用方按 header 长度读就会截断。
func synthResponse(status int, header http.Header, body []byte) *http.Response {
	h := http.Header{}
	for k, vs := range header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Content-Encoding":
			continue // 已被我们改写/已解压，不能沿用上游的值
		}
		h[k] = vs
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		StatusCode:    status,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Uncompressed:  true,
	}
}


// recordRateLimitFromBody 把上游限流记进台账（供控制台展示「限到几点」）。
// 从 429 响应体里取业务码、恢复时刻，从 payload 取模型名，从 auth 取 uid/昵称。
// 任何一步拿不到都安静跳过——记录失败绝不能影响请求本身。
func recordRateLimitFromBody(auth AuthProvider, payload, body []byte) {
	if auth == nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return
	}
	code := 0
	switch v := m["code"].(type) {
	case float64:
		code = int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			code = int(n)
		}
	}
	msg, _ := m["msg"].(string)

	// 模型名：从请求体里取（上游 429 不带模型信息）
	model := ""
	var req map[string]any
	if json.Unmarshal(payload, &req) == nil {
		model, _ = req["model"].(string)
	}

	uid, nick := "", ""
	if sum, err := auth.Summary(); err == nil {
		uid, _ = sum["uid"].(string)
		nick, _ = sum["nickname"].(string)
	}
	if uid == "" {
		return
	}

	// 只记额度类信号，其它上游 4xx 一律不进台账（见 isQuotaSignal）。
	if !isQuotaSignal(code, msg) {
		return
	}

	kind, reason := rlKindModel, "限流"
	if code == 11140 {
		// 11140 实测是账号级：连 hi/1+1 都被拒（2026-10-12 验证cred-a），
		// 与内容无关，故记为账号级（全模型不可用）。
		kind, reason = rlKindAccount, "账号级风控(11140)"
	} else if strings.Contains(msg, "frequency limit") {
		reason = "用量限流(6004)"
	} else if code == 14003 {
		reason = "并发限流(14003)"
	}

	resetAt := time.Time{}
	if t, ok := parseResetAt(msg); ok {
		resetAt = t
	}
	rateLimitRecord(uid, nick, kind, model, code, reason, resetAt)
}

// isQuotaSignal 判断上游 4xx 是不是真的「额度/频控」，只有这类才配写进限流台账。
//
// 台账回答的是「这个账号限到几点」，请求本身的问题必须排除，否则控制台会拿一个
// 兜底恢复时刻盖掉真实原因（2026-10-13：桥接层把并行工具结果排列弄坏，上游报
// 400 code 11148，却被记成「deepseek-v4.1-flash 限流到 10:58」）。
//
// 收录 6004（用量频控，文案带 reset at）、14003（too many requests）、
// 11140（账号级风控）。排除 11115 上下文超长、11128 渠道风控/首条消息、
// 11133 参数被拒、11148 工具记录不完整、11150 思考档位不支持。
func isQuotaSignal(code int, msg string) bool {
	switch code {
	case 6004, 14003, 11140:
		return true
	}
	// 上游换码但文案仍写明频控的，按文案放行。
	return strings.Contains(msg, "frequency limit") || strings.Contains(msg, "too many requests")
}

// ---------- 渠道校验拦截（11128）静默重试 ----------
//
// 2026-10-21 用户报国内线路报：
//
//	400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel",
//	     "displayMsg":{"zh":"请求被拦截，请重新发送"},
//	     "displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的…"}}
//
// 查历史频率：10-10 出现 13 次、10-13 出现 6 次、10-21 出现 1 次 —— 不是
// 持续封禁，是**间歇性的渠道风控**（同一分钟内 msgs=2 的 title 请求能过、
// msgs=22 的对话被拦；隔一会儿再发同一形状又能过）。
//
// 两个直接证据说明「重试就该通过」：
//  1) 上游 displayMsg 自己写的是「请求被拦截，请重新发送」/「Request blocked.
//     Please send it again」——明示客户端重发；
//  2) 实测今天 09:00 之后用同样的请求形状连续 5 次全部通过。
//
// 而 hub 之前把 400 原样透传，用户看到报错只能手动重发。这里在 hub 内部
// 静默重试，客户端无感。等待比 429 短（1.5s 递增），因为它是随机拦截而非
// 限流窗口，长等无意义。

// channelBlockCode 渠道校验拦截的业务码。
const channelBlockCode = 11128

// channelBlockRetriesDefault 默认**不**在 hub 内静默重试。
//
// 2026-10-21 曾设 3（一次客户端请求最多打 4 次上游），依据是上游 displayMsg
// 自己写「请重新发送」。2026-10-23 上游收紧非官方客户端识别后这个判断要反过来：
// 11128 不再只是随机误拦，它开始和账号风险挂钩（社区反馈反复命中后账号进入
// 11140）。在 hub 里把一个可疑请求连打 4 次，等于主动把风险信号放大 4 倍；
// 而真正的根因（请求指纹 / 身份句）已经由 officialChatHeaders 与
// sanitizeHarnessPrompt 处理。
//
// 确需旧行为时用 AIHUB_CHANNEL_RETRY=N 显式打开（上限 3）；
// AIHUB_NO_CHANNEL_RETRY=1 仍然强制关闭。
const channelBlockRetriesDefault = 0
const channelBlockRetriesMax = 3

// channelBlockRetryEnv 覆盖默认重试次数。
const channelBlockRetryEnv = "AIHUB_CHANNEL_RETRY"

// channelBlockRetries 当前生效的重试次数。
func channelBlockRetries() int {
	if channelBlockDisabled() {
		return 0
	}
	v := strings.TrimSpace(os.Getenv(channelBlockRetryEnv))
	if v == "" {
		return channelBlockRetriesDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return channelBlockRetriesDefault
	}
	if n > channelBlockRetriesMax {
		n = channelBlockRetriesMax
	}
	return n
}

// channelBlockWait 第 n 次重试前的等待：1.5s / 3s / 4.5s。
func channelBlockWait(attempt int) time.Duration {
	return time.Duration(attempt) * 1500 * time.Millisecond
}

// channelBlockDisabled 环境变量关闭开关（设为 1 即强制不重试）。
const channelBlockDisabledEnv = "AIHUB_NO_CHANNEL_RETRY"

func channelBlockDisabled() bool {
	return strings.TrimSpace(os.Getenv(channelBlockDisabledEnv)) == "1"
}

// markChannelBlock 把这次 11128 记进渠道拦截窗口；累计到阈值就开账号闸门。
// 返回闸门截止时刻与是否已开闸门。记录失败不影响请求本身。
func markChannelBlock(auth AuthProvider) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	sum, err := auth.Summary()
	if err != nil {
		return time.Time{}, false
	}
	uid, _ := sum["uid"].(string)
	nick, _ := sum["nickname"].(string)
	return rateLimitRecordChannel(uid, nick)
}

// isChannelBlockBody 判断响应体是不是 11128 渠道拦截。
// 只认 status 400 + 明确业务码，避免把别的 400 也拿去重试（比如 11115
// prompt too long 重试多少次都没用）。
func isChannelBlockBody(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	if !bytes.Contains(body, []byte(`"code":11128`)) {
		return false
	}
	// 同码还有 "conversation id missing" / "first message is not system prompt"
	// 两种 msg（见 converter-intl.log 历史）。前者是缺会话四头，重试无用，
	// 该由 withConversationHeaders 解决；后者是消息形状问题，重试也白搭。
	// 只对「渠道未授权」这一类重试。
	return bytes.Contains(body, []byte("unapproved channel"))
}

// retryChannelBlock 命中 11128 时在内部重试；返回最终的 resp。
//
// 注意流式：这里只处理「还没向客户端写过任何字节」的阶段（resp 拿回来、
// 状态码非 200 的分支），所以重试安全。若 11128 出现在 200 之后的 SSE 帧里
// （intl 2026-10-16 踩过这种形态），本函数管不到，由上层错误帧逻辑处理。
func (p *Proxy) retryChannelBlock(ctx context.Context, client *http.Client, auth AuthProvider, url string, headers map[string]string, payload []byte, logger *FileLogger, rid string, resp *http.Response, err error) (*http.Response, error) {
	if err != nil || resp == nil {
		return resp, err
	}
	retries := channelBlockRetries()
	for attempt := 1; ; attempt++ {
		if resp.StatusCode != http.StatusBadRequest {
			return resp, nil
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, retry429BodyLimit))
		resp.Body.Close()
		if rerr != nil || !isChannelBlockBody(resp.StatusCode, body) {
			// 不是渠道拦截：把 body 原样放回去，交给后续逻辑。
			// 2026-10-21 开源审查修：只换了 Body 没同步 ContentLength，
			// 下游 io.Copy / 客户端会按旧长度读，出现提前 EOF 或多读垃圾。
			setRespBody(resp, body)
			return resp, nil
		}
		if attempt > retries {
			until, gated := markChannelBlock(auth)
			if gated {
				logger.Log("[%s] ✗ 渠道校验拦截（11128）重试 %d 次仍失败；10 分钟内累计 %d 次，"+
					"账号闸门开到 %s | %s",
					rid, attempt-1, channelBlockGateHits, rateLimitShortUntil(until), truncate(string(body), 160))
			} else {
				logger.Log("[%s] ✗ 渠道校验拦截（11128）重试 %d 次仍失败 | %s",
					rid, attempt-1, truncate(string(body), 160))
			}
			setRespBody(resp, annotateChannelBlockGiveUp(body, attempt-1, gated, until))
			return resp, nil
		}
		wait := channelBlockWait(attempt)
		logger.Log("[%s] ⏳ 渠道校验拦截（11128），%.1fs 后重试（第 %d/%d 次）",
			rid, wait.Seconds(), attempt, retries)
		if !sleepCtx(ctx, wait) {
			logger.Log("[%s] ✗ 渠道拦截重试等待中被取消", rid)
			setRespBody(resp, body)
			return resp, nil
		}
		var nerr error
		resp, nerr = p.doUpstream(ctx, client, auth, url, headers, payload)
		if nerr != nil {
			return nil, nerr
		}
		if resp.StatusCode == http.StatusOK {
			logger.Log("[%s] ✓ 渠道校验重试成功（第 %d 次，共等 %.1fs）",
				rid, attempt, float64(attempt)*1.5)
			return resp, nil
		}
	}
}

// annotateChannelBlockGiveUp 重试也失败时，给客户端一句能行动的说明。
func annotateChannelBlockGiveUp(body []byte, attempts int, gated bool, until time.Time) []byte {
	var note string
	switch {
	case gated:
		note = fmt.Sprintf("\n\n[hub] 渠道校验拦截（11128）：10 分钟内已累计 %d 次，"+
			"本账号已暂停出站到 %s（本地闸门，上游未给恢复时刻）。"+
			"请改用其它线路/模型；恢复后若再次命中，说明请求形状仍被识别，需要更新官方客户端版本或换账号。",
			channelBlockGateHits, rateLimitShortUntil(until))
	case attempts == 0:
		note = "\n\n[hub] 渠道校验拦截（11128）：hub 已默认不再静默重试（反复重试会放大风险信号）。" +
			"请稍后手动重发或改用其它线路；若持续出现，先核对官方客户端版本是否被上游更新。"
	default:
		note = fmt.Sprintf("\n\n[hub] 渠道校验拦截（11128）：已在 hub 内部重试 %d 次仍未通过。"+
			"请改用其它线路，或等几分钟再手动重发。", attempts)
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return append(body, []byte(note)...)
	}
	if dm, ok := m["displayMsg"].(map[string]any); ok {
		if zh, ok := dm["zh"].(string); ok {
			dm["zh"] = zh + note
		} else {
			dm["zh"] = strings.TrimPrefix(note, "\n\n")
		}
	} else {
		m["hub_hint"] = strings.TrimPrefix(note, "\n\n")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// setRespBody 把已读出的字节塞回 resp.Body，并同步 ContentLength。
//
// 2026-10-21 开源审查修：原来只换 Body、ContentLength 还是原始值，而
// annotateChannelBlockGiveUp 会在 JSON 尾部追加提示文字让 body 变长。
// 下游若按 ContentLength 读就会提前 EOF 或截断。
func setRespBody(resp *http.Response, b []byte) {
	if resp == nil {
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	if resp.Header != nil {
		resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}
}
