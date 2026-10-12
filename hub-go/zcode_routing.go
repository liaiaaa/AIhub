package main

// 端点路由（2026-09-27 自 TriDefender/zcode-api 的 endpoint-routing.ts 逆向移植，
// 对齐 ZCode 3.7+ ProviderEndpointRoutingService）：
//
// 桌面端定期 GET {zcodeApiBase}/api/v1/agent/configs，按返回
// data.proxyEndpoint.mapping（from→to，归一化 URL 精确匹配）改写 provider
// 请求 URL。表由服务端控制、随时可增删（2026-08-19 曾把 coding-plan 端点
// 改写到 zcode.z.ai/api/v1/ultra[-zai]/...）；2026-09-27 实测 HTTP 200、
// 当期 mapping 为空，故本实现必须且已做到无表时零影响。
//
// 语义严格 fail-open：任何拉取/解析失败保留旧快照（或无快照）并进入 30s
// 冷却，请求按原 URL 直发——路由层永远不能成为故障点。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	zcodeRoutingOrigin      = "https://zcode.z.ai"
	zcodeRoutingConfigPath  = "/api/v1/agent/configs"
	zcodeRoutingSuccessTTL  = 5 * time.Minute
	zcodeRoutingFailCool    = 30 * time.Second
	zcodeRoutingTimeout     = 3 * time.Second
	zcodeRoutingMaxMappings = 256
)

// zcodeRouting 服务端下发的端点改写表（进程级单例，随代理启停）。
type zcodeRouting struct {
	origin     string
	identity   *zcodeIdentity
	credential func() string // coding-plan API Key（配置拉取的 x-api-key）

	mu         sync.Mutex
	mapping    map[string]string
	expiresAt  time.Time
	retryAfter time.Time
	client     *http.Client
}

func newZcodeRouting(identity *zcodeIdentity, credential func() string) *zcodeRouting {
	return &zcodeRouting{
		origin:     zcodeRoutingOrigin,
		identity:   identity,
		credential: credential,
		client:     &http.Client{Transport: directClient().Transport, Timeout: zcodeRoutingTimeout},
	}
}

// zcodeRoutingKey 归一化路由键：scheme//host:port(缺省443)/path 去尾斜杠。
func zcodeRoutingKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	path := strings.TrimRight(u.Path, "/")
	return strings.ToLower(u.Scheme) + "//" + strings.ToLower(u.Hostname()) + ":" + port + path
}

// resolve 请求 URL 过改写表。永不报错：任何一步失败都原样返回。
// 命中形态两种（服务端表两种都发过）：完整请求 URL 精确匹配，或
// provider base URL 前缀匹配（剩余路径与查询串原样接到目标）。
func (r *zcodeRouting) resolve(rawURL string) string {
	if r == nil {
		return rawURL
	}
	key := zcodeRoutingKey(rawURL)
	if key == "" {
		return rawURL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.expiresAt.IsZero() && now.Before(r.expiresAt) {
		return r.lookupLocked(rawURL, key)
	}
	if !r.retryAfter.IsZero() && now.Before(r.retryAfter) {
		return rawURL
	}
	if err := r.refreshLocked(); err != nil {
		r.retryAfter = time.Now().Add(zcodeRoutingFailCool)
		return rawURL
	}
	return r.lookupLocked(rawURL, key)
}

// lookupLocked 查改写表：精确键 → 按路径段向上收缩的前缀键。
// 调用方持锁。未命中返回原 URL。
func (r *zcodeRouting) lookupLocked(rawURL, key string) string {
	if to, ok := r.mapping[key]; ok {
		return keepQuery(rawURL, to)
	}
	// 前缀匹配：路径段逐级上收（base 表项 + 剩余路径拼接）
	prefix := key
	for {
		i := strings.LastIndex(prefix, "/")
		if i < 0 || strings.HasSuffix(prefix[:i+1], "//") {
			break // 只剩 scheme//host，无路径可收
		}
		prefix = prefix[:i]
		suffix := key[len(prefix):] // 以 / 开头的剩余路径
		if to, ok := r.mapping[prefix]; ok {
			return keepQuery(rawURL, to+suffix) // keepQuery 自行从 rawURL 取查询串
		}
	}
	return rawURL
}

// queryOf 返回 rawURL 的 ?query 部分（含问号），无则空串。
func queryOf(rawURL string) string {
	if i := strings.Index(rawURL, "?"); i >= 0 {
		return rawURL[i:]
	}
	return ""
}

// keepQuery 目标 URL 换路径保留原查询串（对齐参考实现的 rewritten.search）。
func keepQuery(rawURL, to string) string {
	if q := queryOf(rawURL); q != "" {
		if strings.Contains(to, "?") {
			return to + "&" + q[1:]
		}
		return to + q
	}
	return to
}

// refreshLocked 拉取并解析 agent/configs（调用方持锁）。任何异常都返回错误、
// 不动现有 mapping。
func (r *zcodeRouting) refreshLocked() error {
	req, err := http.NewRequest("GET", r.origin+zcodeRoutingConfigPath+
		"?app_version="+url.QueryEscape(r.identity.appVersion)+
		"&platform="+url.QueryEscape(r.identity.platformParam()), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	setIdentityHeaders(req, r.identity.contextHeaders())
	if r.credential != nil {
		if key := strings.TrimSpace(r.credential()); key != "" {
			req.Header.Set("x-api-key", key)
		}
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ProxyEndpoint struct {
				Mapping []struct {
					From string `json:"from"`
					To   string `json:"to"`
				} `json:"mapping"`
			} `json:"proxyEndpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return err
	}
	if parsed.Code != 0 {
		return &upstreamCodeError{Code: parsed.Code, Msg: parsed.Msg}
	}
	list := parsed.Data.ProxyEndpoint.Mapping
	if len(list) > zcodeRoutingMaxMappings {
		return errZcodeRoutingTooMany
	}
	mapping := make(map[string]string, len(list))
	for _, e := range list {
		if !isPlainHTTPSRewrite(e.From) || !isPlainHTTPSRewrite(e.To) {
			return errZcodeRoutingBadEntry
		}
		from := zcodeRoutingKey(e.From)
		if from == "" {
			return errZcodeRoutingBadEntry
		}
		if _, dup := mapping[from]; dup {
			return errZcodeRoutingBadEntry
		}
		mapping[from] = strings.TrimRight(e.To, "/")
	}
	r.mapping = mapping
	r.expiresAt = time.Now().Add(zcodeRoutingSuccessTTL)
	r.retryAfter = time.Time{}
	return nil
}

// isPlainHTTPSRewrite 映射表条目必须是纯 https URL（无 userinfo/query/fragment）。
func isPlainHTTPSRewrite(v string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}
	return u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// SnapshotSize 当前映射条数（health 展示用）。
func (r *zcodeRouting) SnapshotSize() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.mapping)
}

// HasSnapshot 是否已有成功拉取的快照。
func (r *zcodeRouting) HasSnapshot() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.expiresAt.IsZero()
}

// sentinel 解析错误（统一进 30s 冷却）。
var (
	errZcodeRoutingTooMany  = &zcodeRoutingError{"映射条目过多"}
	errZcodeRoutingBadEntry = &zcodeRoutingError{"映射条目非法"}
)

type zcodeRoutingError struct{ msg string }

func (e *zcodeRoutingError) Error() string { return "agent/configs " + e.msg }

type httpStatusError struct{ Status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

type upstreamCodeError struct {
	Code int
	Msg  string
}

func (e *upstreamCodeError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}
