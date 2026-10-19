package main

// 后端请求必须用带「死代理回落」的 client。
//
// 2026-10-17 的现场：aihub 进程环境里留着 https_proxy=http://127.0.0.1:40643，
// 而那个端口早没人听了（start.sh 会把 shell 的代理变量带进服务环境）。
// 推理线路走 newBackendClient()，代理不可达会自动改直连；但看板探活给每个凭证
// 新建一个 Proxy，runtime() 的 client 是 nil，于是落到 &http.Client{Timeout:20s}
// 这个兜底 —— 裸 client 用 http.DefaultTransport，无条件信环境变量，
// 结果 proxyconnect 失败，三个好凭证全被标成「探活失败」，看板显示 0/4 可用。
//
// 这类 bug 光靠肉眼看代码扫不干净（同样的兜底我当时在 auth.go / checkin.go /
// model_probe.go / auth_mimo.go 里一共埋了 6 处），所以用源码检查钉住。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 允许保留裸 client 的位置：只打 127.0.0.1 回环，no_proxy 覆盖得到，
// 走不走环境变量代理没有区别。加条目必须同时确认目标确实是回环地址。
// 键用「文件名 + 那一行的原文」而不是行号 —— 行号会随上面加几行注释就漂掉，
// 漂了守卫就变成要么误报要么漏报。
var bareClientAllowed = map[string]bool{
	`proxy_agg.go|client := &http.Client{Timeout: 5 * time.Second}`:        true, // http://127.0.0.1:<线路端口>/v1/models
	`proxy_anthropic.go|return &http.Client{Timeout: 10 * time.Minute}`:    true, // aggHTTPClient：网关到线路本地端口
	`zcode_relay.go|client := &http.Client{Timeout: zcodeRelayProbeWait}`:  true, // zcodeRelayProbeURL = http://127.0.0.1:8807
}

func TestNoBareHTTPClientOnRemotePaths(t *testing.T) {
	re := regexp.MustCompile(`&http\.Client\{Timeout`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // 注释里的反例说明
			}
			if f == "proxy_util.go" {
				continue // newBackendClient / backendClient 本体
			}
			loc := f + "|" + strings.TrimSpace(line)
			seen++
			if !bareClientAllowed[loc] {
				t.Errorf("%s 用了裸 http.Client —— 打后端的请求必须用 backendClient()/newBackendClient()，"+
					"否则环境变量里一个没人听的代理就会把它打挂：%s", loc, strings.TrimSpace(line))
			}
		}
	}
	if seen == 0 {
		t.Fatal("一处都没扫到，检查正则是不是失效了")
	}
}

func TestBackendClientHasProxyFallback(t *testing.T) {
	c := backendClient(5 * time.Second)
	if c.Timeout != 5*time.Second {
		t.Errorf("超时没设上：%v", c.Timeout)
	}
	// 关键：不能是 DefaultTransport —— 它无条件信环境变量代理，
	// 死代理就是把看板探活全打挂的那个原因。
	if c.Transport == nil || c.Transport == http.DefaultTransport {
		t.Fatal("backendClient 必须自带 Proxy 回落逻辑，不能退回 DefaultTransport")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型意外：%T", c.Transport)
	}
	if tr.Proxy == nil {
		t.Error("Proxy 判定函数没设，等于没有死代理回落")
	}
	// 真发一次请求确认可用（回环服务，不碰上游）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("backendClient 请求失败：%v", err)
	}
	resp.Body.Close()
}
