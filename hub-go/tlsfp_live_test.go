package main

// 真机验证（默认跳过）：
//   TLSFP_LIVE_TEST=1     go test -run TestLiveTLSFingerprintJA3 -v
//   HEARTBEAT_LIVE_TEST=1 go test -run TestLiveHeartbeatProbeIntl -v
//
// 两条都只发只读请求：
//   - JA3 对比打 tls.peet.ws（第三方回显服务，不涉及任何上游账号）；
//   - 心跳探针走 intl 的 GET /v3/config（官方桌面端本来就会定时拉的那条）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

func peekJA3(t *testing.T, cli *http.Client) (ja3 string) {
	t.Helper()
	cli.Timeout = 25 * time.Second
	resp, err := cli.Get("https://tls.peet.ws/api/all")
	if err != nil {
		t.Skipf("拉 tls.peet.ws 失败（可能没网/代理没开），跳过：%v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Skipf("回显服务响应不是 JSON，跳过：%v", err)
	}
	tlsm, _ := m["tls"].(map[string]any)
	ja3, _ = tlsm["ja3_hash"].(string)
	t.Logf("proto=%s ja3_hash=%s ja3=%v", resp.Proto, ja3, tlsm["ja3"])
	return ja3
}

func TestLiveTLSFingerprintJA3(t *testing.T) {
	if os.Getenv("TLSFP_LIVE_TEST") != "1" {
		t.Skip("跳过真机 TLS 指纹验证；设置 TLSFP_LIVE_TEST=1 启用")
	}
	utlsJA3 := peekJA3(t, newBackendClientFor("intl"))
	if utlsJA3 == "" {
		t.Fatal("uTLS 客户端没拿到 ja3_hash")
	}

	// 对照组：同样的代理逻辑，但不套 uTLS（Go 默认指纹）
	tr := &http.Transport{Proxy: func(req *http.Request) (*url.URL, error) {
		return resolveRequestProxy(req), nil
	}}
	goJA3 := peekJA3(t, &http.Client{Transport: tr})
	if goJA3 == "" {
		t.Fatal("Go 默认客户端没拿到 ja3_hash")
	}
	if utlsJA3 == goJA3 {
		t.Fatalf("uTLS 与 Go 默认的 JA3 相同（%s）—— 指纹伪装没生效", goJA3)
	}
	t.Logf("指纹差异确认：utls=%s go=%s", utlsJA3, goJA3)
}

func TestLiveHeartbeatProbeIntl(t *testing.T) {
	if os.Getenv("HEARTBEAT_LIVE_TEST") != "1" {
		t.Skip("跳过真机心跳探针验证；设置 HEARTBEAT_LIVE_TEST=1 启用")
	}
	hubRoot = repoRoot(t)
	dir := ""
	for _, d := range credDirs() {
		dir = d
		break
	}
	if dir == "" {
		t.Skip("没有凭证目录，跳过")
	}
	_ = NewHub() // 保持与生产一致的初始化路径（loadState 等）
	p := NewIntlProxy(dir)
	if path := p.AuthFilePath(); path == "" || !fileExists(path) {
		t.Skip("国际凭证文件不存在，跳过")
	}

	// 与 heartbeatProbes 里的 intl 探针完全同一条代码路径（只读、不落盘）
	n, err := heartbeatWBCatalogPull("intl", p)
	if err != nil {
		t.Fatalf("intl 目录拉取失败（心跳探针会记为失败）：%v", err)
	}
	if n == 0 {
		t.Fatal("目录条目为 0，上游返回结构可能变了")
	}
	t.Logf("intl 心跳探针成功：%d 个条目", n)

	// 顺带确认这次握手走的是 uTLS（统计里有 intl 记录且 ALPN 有值）
	tlsFPStatsMu.Lock()
	st := tlsFPStats["intl"]
	tlsFPStatsMu.Unlock()
	if st == nil || st.Handshakes == 0 {
		t.Fatalf("intl 没有 uTLS 握手记录：%+v", st)
	}
	t.Logf("intl 握手统计：profile=%s alpn=%s tls=%s", st.Profile, st.LastALPN, st.LastVersion)

	// 心跳调度层：把这条探针喂给 tick，确认能被选中并记账
	resetActivity()
	hb := testHeartbeatState(t)
	hb.cfg.IdleGateSec = 0
	hb.cfg.MaxPerDay = 100
	var ran int
	hb.runHeartbeatTick([]heartbeatProbe{{Line: "intl", Label: "上游目录 /v3/config", Run: func(ctx context.Context) (string, error) {
		ran++
		return "ok", nil
	}}})
	if ran != 1 || hb.lines["intl"] == nil || !hb.lines["intl"].LastOK {
		t.Fatalf("心跳调度没有实际执行探针：ran=%d state=%+v", ran, hb.lines["intl"])
	}
}

// TestLiveHeartbeatProbeDom 国内 WorkBuddy 的心跳探针（只读 GET /v3/config）。
// 用户 2026-11-05 特别点名「WorkBuddy 封号最严重」，所以这条单独钉一遍：
// 它验证的是 dom 线路真能走 uTLS chrome + HTTP/1.1 拿到目录。
func TestLiveHeartbeatProbeDom(t *testing.T) {
	if os.Getenv("HEARTBEAT_LIVE_TEST") != "1" {
		t.Skip("跳过真机 dom 心跳探针；设置 HEARTBEAT_LIVE_TEST=1 启用")
	}
	hubRoot = repoRoot(t)
	dir := liveCredDir(t, "CodeBuddy凭证")
	p := NewDomProxyDir(dir)
	if path := p.AuthFilePath(); path == "" || !fileExists(path) {
		t.Skip("dom 凭证文件不存在，跳过")
	}

	n, err := heartbeatWBCatalogPull("dom", p)
	if err != nil {
		t.Fatalf("dom 目录拉取失败（心跳探针会记为失败）：%v", err)
	}
	if n == 0 {
		t.Fatal("目录条目为 0，上游返回结构可能变了")
	}
	t.Logf("dom 心跳探针成功：%d 个条目（backend=%s）", n, p.Backend)

	tlsFPStatsMu.Lock()
	st := tlsFPStats["dom"]
	tlsFPStatsMu.Unlock()
	if st == nil || st.Handshakes == 0 {
		t.Fatalf("dom 没有 uTLS 握手记录：%+v", st)
	}
	t.Logf("dom 握手统计：profile=%s alpn=%s tls=%s", st.Profile, st.LastALPN, st.LastVersion)
}
