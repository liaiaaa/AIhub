package main

// 本地 start-plan 中继（2026-09-27 接入，免费额度通道）。
//
// 背景：Start Plan / 活动赠送额度只能经 zcode.z.ai 私有中继消费，网关强制
// 阿里云验证码（3007），验证码求解器属于持续的风控对抗，不在 Go 代理里
// 自研；改用 MIT 开源工程 TriDefender/zcode-api 作为本地引擎
// （tools\zcode-relay\，Bun 运行时，凭据注入见 inject-credential.mjs）。
// 该引擎在本机 8807 暴露 OpenAI 兼容接口，内部完成验证码求解 + JWT 认证 +
// OpenAI→Anthropic 转译。
//
// 接入语义：
//   - 自动探测：Start 时及后台每 5 分钟探测一次中继（探测成功即切换），
//     ZCODE_RELAY_URL 环境变量可指定其他地址，"off"/"direct"/"none" 强制关闭；
//     ZCODE_RELAY_KEY 覆盖本地密钥。
//   - 不自动回退：中继请求失败时报错并提示启动中继，绝不静默回落到
//     api.z.ai 直连——直连是充值余额计费通道，静默回落会烧真钱。
//
// 本地密钥：中继对浏览器发宽松 CORS（其 MCP/webui 设计如此），密钥若可猜测，
// 恶意网页可驱动本机中继烧免费额度。密钥解析顺序（见 zcodeRelayKey）：
// env ZCODE_RELAY_KEY > %USERPROFILE%\.zcode-proxy\relay-key（inject 脚本
// 随机生成并与 config.yaml 同步）> 历史常量兜底。

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// zcodeRelayKey 中继本地密钥（进程级缓存）。
var (
	zcodeRelayKeyOnce     sync.Once
	zcodeRelayKeyValue    string
	zcodeRelayKeyFallback = "hub-zcode-relay"
)

func zcodeRelayKey() string {
	zcodeRelayKeyOnce.Do(func() {
		if k := strings.TrimSpace(os.Getenv("ZCODE_RELAY_KEY")); k != "" {
			zcodeRelayKeyValue = k
			return
		}
		home, err := os.UserHomeDir()
		if err == nil {
			if b, rerr := os.ReadFile(filepath.Join(home, ".zcode-proxy", "relay-key")); rerr == nil {
				if k := strings.TrimSpace(string(b)); k != "" {
					zcodeRelayKeyValue = k
					return
				}
			}
		}
		zcodeRelayKeyValue = zcodeRelayKeyFallback
	})
	return zcodeRelayKeyValue
}

// detectRelay 探测本地中继并落定 relayURL/relayKey。force=true 用于 Start
// （忽略冷却）；后台周期调用时带冷却，探测失败不影响现有转发模式。
func (p *ZcodeProxy) detectRelay(force bool) {
	p.relayProbeMutex.Lock()
	defer p.relayProbeMutex.Unlock()
	if !force && !p.relayProbeAt.IsZero() && time.Since(p.relayProbeAt) < zcodeRelayProbeTTL {
		return
	}
	p.relayProbeAt = time.Now()

	if url := strings.TrimSpace(os.Getenv("ZCODE_RELAY_URL")); url != "" {
		if strings.EqualFold(url, "off") || strings.EqualFold(url, "direct") || strings.EqualFold(url, "none") {
			p.relayURL = ""
			return
		}
		p.relayURL = strings.TrimRight(url, "/")
		p.relayKey = zcodeRelayKey()
		return
	}
	// 自动探测：GET /v1/models，中继要求本地密钥
	probe := zcodeRelayProbeURL + "/v1/models"
	ctx, cancel := context.WithTimeout(context.Background(), zcodeRelayProbeWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", probe, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+zcodeRelayKey())
	client := &http.Client{Timeout: zcodeRelayProbeWait}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		p.relayURL = zcodeRelayProbeURL
		p.relayKey = zcodeRelayKey()
		p.logf("检测到本地 start-plan 中继（%s），免费额度通道已启用", p.relayURL)
	}
}

// relayTarget 中继模式的请求目标与密钥（未启用返回 ok=false）。
func (p *ZcodeProxy) relayTarget() (url, key string, ok bool) {
	p.relayProbeMutex.Lock()
	defer p.relayProbeMutex.Unlock()
	if p.relayURL == "" {
		return "", "", false
	}
	return p.relayURL, p.relayKey, true
}

// ---- 请求启动节流（防验证码风控的爆发式特征） ----
//
// 风控盯的是验证码请求的频率与节奏。dsh 是 agent：工具调用并行连发，
// 请求启动时刻密集；这里把中继上游请求的「启动时刻」拉平到最小间隔
// （默认 2s），向人类节奏靠拢。注意只节流启动、不限制并发生成流——
// dsh 并行工具调用的长流（30s~数分钟）不会被互相卡住。
//
// 环境变量 ZCODE_RELAY_START_INTERVAL_MS 可调（0 = 关闭）。

var (
	zcodeRelayIntervalOnce sync.Once
	zcodeRelayIntervalVal  time.Duration
)

func zcodeRelayStartInterval() time.Duration {
	zcodeRelayIntervalOnce.Do(func() {
		zcodeRelayIntervalVal = 2 * time.Second
		if v := strings.TrimSpace(os.Getenv("ZCODE_RELAY_START_INTERVAL_MS")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				zcodeRelayIntervalVal = time.Duration(n) * time.Millisecond
			}
		}
	})
	return zcodeRelayIntervalVal
}

// relayWaitStart 中继模式下请求启动前的排队点（ctx 可取消）。
func (p *ZcodeProxy) relayWaitStart(ctx context.Context) {
	interval := zcodeRelayStartInterval()
	if interval <= 0 {
		return
	}
	p.relayStartMu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if p.relayNextStart.After(now) {
		wait = p.relayNextStart.Sub(now)
	}
	p.relayNextStart = now.Add(wait + interval)
	p.relayStartMu.Unlock()
	if wait <= 0 {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
