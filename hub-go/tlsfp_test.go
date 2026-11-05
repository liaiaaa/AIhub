package main

// 出站 TLS 指纹（tlsfp.go）的离线测试。
//
// 全部在本机起 httptest / 假代理，不出网：
//   - 档位选择与环境变量覆盖
//   - applyTLSFingerprint 确实接管了拨号层（Proxy 关掉、DialTLSContext 装上、h2 打开）
//   - 真握手 + ALPN 协商出 h2（Chrome 档）
//   - HTTP CONNECT 隧道 + 隧道内握手
//   - SOCKS5 问候/认证/CONNECT 应答的解析

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func clearTLSFPEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{tlsFPEnvGlobal, tlsFPEnvFallback, tlsFPEnvDebug,
		"AIHUB_HTTP2", "AIHUB_HTTP2_DOM", "AIHUB_HTTP2_INTL", "AIHUB_HTTP2_OC",
		tlsFPEnvLinePfx + "DOM", tlsFPEnvLinePfx + "INTL", tlsFPEnvLinePfx + "OC"} {
		t.Setenv(k, "")
	}
}

func TestTLSFPProfileSelection(t *testing.T) {
	clearTLSFPEnv(t)

	if spec, ok := tlsFPProfileFor("dom"); !ok || spec.Key != "chrome" {
		t.Fatalf("dom 默认应 chrome，得到 %q ok=%v", spec.Key, ok)
	}
	if spec, ok := tlsFPProfileFor("oc"); !ok || spec.Key != "golang" {
		t.Fatalf("oc 默认应 golang（CLI 身份），得到 %q ok=%v", spec.Key, ok)
	}

	t.Setenv(tlsFPEnvGlobal, "firefox")
	if spec, _ := tlsFPProfileFor("dom"); spec.Key != "firefox" {
		t.Fatalf("全局覆盖失效，得到 %q", spec.Key)
	}
	// 按线路覆盖优先于全局
	t.Setenv(tlsFPEnvLinePfx+"INTL", "safari")
	if spec, _ := tlsFPProfileFor("intl"); spec.Key != "safari" {
		t.Fatalf("按线路覆盖失效，得到 %q", spec.Key)
	}
	if spec, _ := tlsFPProfileFor("dom"); spec.Key != "firefox" {
		t.Fatalf("未覆盖的线路应沿用全局，得到 %q", spec.Key)
	}

	// off 关掉伪装
	t.Setenv(tlsFPEnvLinePfx+"INTL", "off")
	if _, ok := tlsFPProfileFor("intl"); ok {
		t.Fatal("INTL=off 应返回未启用")
	}

	// 未知档位：回落到默认档而不是静默关闭
	t.Setenv(tlsFPEnvLinePfx+"DOM", "netscape")
	if spec, ok := tlsFPProfileFor("dom"); !ok || spec.Key != "chrome" {
		t.Fatalf("未知档位应回落默认 chrome，得到 %q ok=%v", spec.Key, ok)
	}
}

func TestApplyTLSFingerprintWiresTransport(t *testing.T) {
	clearTLSFPEnv(t)
	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	if !applyTLSFingerprint(tr, "intl") {
		t.Fatal("intl 应启用指纹伪装")
	}
	if tr.Proxy != nil {
		t.Fatal("接管拨号层后必须把 net/http 的 Proxy 关掉（否则走代理时 DialTLSContext 根本不被调用）")
	}
	if tr.DialTLSContext == nil {
		t.Fatal("DialTLSContext 未安装")
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext 未安装")
	}
	// 默认按客户端抓包走 HTTP/1.1（见 client_profile.go 文件头）。
	if tr.ForceAttemptHTTP2 {
		t.Fatal("默认不应打开 h2（画像默认 http/1.1）")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("关 h2 必须给非 nil 空 TLSNextProto（nil = 让标准库注入默认 h2）")
	}
	if _, ok := tr.TLSNextProto["h2"]; ok {
		t.Fatal("TLSNextProto 里不应有 h2 条目")
	}

	// 需要 h2 时用环境变量开回来
	t.Setenv("AIHUB_HTTP2", "1")
	tr3 := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	if !applyTLSFingerprint(tr3, "intl") {
		t.Fatal("intl 应启用指纹伪装")
	}
	if !tr3.ForceAttemptHTTP2 {
		t.Fatal("AIHUB_HTTP2=1 时应打开 ForceAttemptHTTP2")
	}
	if tr3.TLSNextProto != nil {
		t.Fatal("h2 打开时不应改 TLSNextProto")
	}
	t.Setenv("AIHUB_HTTP2", "")

	// off：transport 保持原样
	t.Setenv(tlsFPEnvGlobal, "off")
	tr2 := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	if applyTLSFingerprint(tr2, "intl") {
		t.Fatal("AIHUB_TLS_FP=off 时不应接管")
	}
	if tr2.DialTLSContext != nil {
		t.Fatal("off 时不应改 DialTLSContext")
	}
}

// startTLSTestServer 起一个自签证书 + h2 的本地服务，并把它的根证书注入测试配置。
func startTLSTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func()) {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	// 显式给出两条 ALPN，才能验证「客户端只报 http/1.1 时服务端就选 http/1.1」。
	// 不设的话 httptest 的 NextProtos 只有 h2，客户端报 http/1.1 会「无交集」——
	// 服务端不报 ALPN 直接按 1.1 走，看起来像协商失败（实测踩过）。
	srv.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	srv.StartTLS()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	prev := tlsFPTestConfigHook
	host := strings.TrimPrefix(srv.URL, "https://")
	serverName := strings.Split(host, ":")[0]
	tlsFPTestConfigHook = func(h string) *utls.Config {
		return &utls.Config{RootCAs: pool, ServerName: serverName}
	}
	return srv, func() {
		tlsFPTestConfigHook = prev
		srv.Close()
	}
}

func TestUTLSHandshakeNegotiatesHTTP2(t *testing.T) {
	clearTLSFPEnv(t)
	t.Setenv("AIHUB_HTTP2", "1") // 显式开 h2，验证 ConnectionState 适配没退化
	var gotProto string
	srv, cleanup := startTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
	})
	defer cleanup()

	tr := &http.Transport{}
	if !applyTLSFingerprint(tr, "intl") {
		t.Fatal("未启用指纹")
	}
	cli := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := cli.Get(srv.URL)
	if err != nil {
		t.Fatalf("uTLS 请求失败：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("响应体不对：%q", body)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("ALPN 未协商出 h2，实际 %s（ConnectionState 适配没生效？）", resp.Proto)
	}
	if gotProto != "HTTP/2.0" {
		t.Fatalf("服务端看到的是 %s", gotProto)
	}
	// resp.TLS 来自我们搬运的 ConnectionState，必须不是 nil
	if resp.TLS == nil || resp.TLS.NegotiatedProtocol != "h2" {
		t.Fatalf("resp.TLS 不可用：%+v", resp.TLS)
	}
}

func TestUTLSHandshakeDefaultsToHTTP1(t *testing.T) {
	clearTLSFPEnv(t)
	var gotProto string
	srv, cleanup := startTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		io.WriteString(w, "ok")
	})
	defer cleanup()

	tr := &http.Transport{}
	if !applyTLSFingerprint(tr, "intl") {
		t.Fatal("未启用指纹")
	}
	cli := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := cli.Get(srv.URL)
	if err != nil {
		t.Fatalf("uTLS 请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("默认应走 HTTP/1.1，实际 %s", resp.Proto)
	}
	if gotProto != "HTTP/1.1" {
		t.Fatalf("服务端看到的是 %s", gotProto)
	}
	if resp.TLS == nil || resp.TLS.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("ALPN 应协商成 http/1.1：%+v", resp.TLS)
	}
}

func TestTLSFPRecordsStats(t *testing.T) {
	clearTLSFPEnv(t)
	tlsFPStatsMu.Lock()
	tlsFPStats = map[string]*tlsFPLineStat{}
	tlsFPStatsMu.Unlock()

	srv, cleanup := startTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	defer cleanup()

	tr := &http.Transport{}
	applyTLSFingerprint(tr, "teleagent")
	cli := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := cli.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	tlsFPStatsMu.Lock()
	st := tlsFPStats["teleagent"]
	tlsFPStatsMu.Unlock()
	if st == nil {
		t.Fatal("没有记录 teleagent 的握手统计")
	}
	if st.Profile != "chrome" || st.Handshakes == 0 || st.LastALPN == "" {
		t.Fatalf("统计不对：%+v", st)
	}
}

// startFakeConnectProxy 起一个只会 CONNECT 的假代理，接受后把流量转发到 target。
func startFakeConnectProxy(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("假代理监听失败：%v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := make([]byte, 4096)
				n, err := c.Read(br)
				if err != nil || n == 0 {
					return
				}
				req := string(br[:n])
				if !strings.HasPrefix(req, "CONNECT ") {
					io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
					return
				}
				line := strings.SplitN(req, "\r\n", 2)[0]
				target := strings.Fields(line)[1]
				up, err := net.Dial("tcp", target)
				if err != nil {
					io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { io.Copy(up, c); done <- struct{}{} }()
				go func() { io.Copy(c, up); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); wg.Wait() }
}

func TestHTTPProxyConnectTunnelHandshake(t *testing.T) {
	clearTLSFPEnv(t)
	srv, cleanup := startTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "tunnel-ok")
	})
	defer cleanup()
	target := strings.TrimPrefix(srv.URL, "https://")

	proxyAddr, closeProxy := startFakeConnectProxy(t)
	defer closeProxy()

	p, _ := url.Parse("http://" + proxyAddr)
	raw, err := dialHTTPProxy(context.Background(), p, target)
	if err != nil {
		t.Fatalf("CONNECT 隧道建立失败：%v", err)
	}
	defer raw.Close()

	// 这条测的是「隧道内的握手」，用 h2 档顺带覆盖 ConnectionState 适配
	d := &tlsFPDialer{line: "intl", profile: "chrome", spec: tlsFPProfiles["chrome"], h2: true}
	conn, st, err := d.handshake(context.Background(), raw, target)
	if err != nil {
		t.Fatalf("隧道内握手失败：%v", err)
	}
	defer conn.Close()
	if st.NegotiatedProtocol != "h2" {
		t.Fatalf("隧道内 ALPN 期望 h2，得到 %q", st.NegotiatedProtocol)
	}
}

func TestHTTPProxyConnectRejectsBadStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1024)
		c.Read(buf)
		io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		time.Sleep(50 * time.Millisecond)
	}()
	p, _ := url.Parse("http://" + ln.Addr().String())
	_, err = dialHTTPProxy(context.Background(), p, "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("应把代理 407 报出来，得到 %v", err)
	}
}

// startFakeSOCKS5 最小 SOCKS5 服务：支持无认证与用户名密码两种，
// CONNECT 后把流量转给 target（由测试决定，这里只连本地 echo）。
func startFakeSOCKS5(t *testing.T, requireAuth bool) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("SOCKS5 监听失败：%v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				head := make([]byte, 2)
				if _, err := io.ReadFull(c, head); err != nil {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, err := io.ReadFull(c, methods); err != nil {
					return
				}
				if requireAuth {
					c.Write([]byte{0x05, 0x02})
					// user/pass 子协商
					ver := make([]byte, 2)
					if _, err := io.ReadFull(c, ver); err != nil {
						return
					}
					user := make([]byte, int(ver[1]))
					io.ReadFull(c, user)
					pl := make([]byte, 1)
					io.ReadFull(c, pl)
					pass := make([]byte, int(pl[0]))
					io.ReadFull(c, pass)
					if string(user) != "u" || string(pass) != "p" {
						c.Write([]byte{0x01, 0x01})
						return
					}
					c.Write([]byte{0x01, 0x00})
				} else {
					c.Write([]byte{0x05, 0x00})
				}
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil {
					return
				}
				var host string
				switch req[3] {
				case 0x01:
					b := make([]byte, 4)
					io.ReadFull(c, b)
					host = net.IP(b).String()
				case 0x03:
					l := make([]byte, 1)
					io.ReadFull(c, l)
					b := make([]byte, int(l[0]))
					io.ReadFull(c, b)
					host = string(b)
				default:
					return
				}
				pb := make([]byte, 2)
				io.ReadFull(c, pb)
				port := int(pb[0])<<8 | int(pb[1])
				_ = host
				// 应答成功，绑定地址 0.0.0.0:0
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				// 之后原样回显，证明隧道通了
				io.Copy(c, c)
				_ = port
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); wg.Wait() }
}

func TestSOCKS5DialNoAuth(t *testing.T) {
	addr, closeFn := startFakeSOCKS5(t, false)
	defer closeFn()
	p, _ := url.Parse("socks5://" + addr)
	conn, err := dialSOCKS5(context.Background(), p, "example.com:443")
	if err != nil {
		t.Fatalf("SOCKS5 无认证失败：%v", err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("Echo 不通：%q err=%v", buf, err)
	}
}

func TestSOCKS5DialWithAuth(t *testing.T) {
	addr, closeFn := startFakeSOCKS5(t, true)
	defer closeFn()
	u := &url.URL{Scheme: "socks5", Host: addr, User: url.UserPassword("u", "p")}
	conn, err := dialSOCKS5(context.Background(), u, "example.com:443")
	if err != nil {
		t.Fatalf("SOCKS5 认证失败：%v", err)
	}
	conn.Close()

	bad := &url.URL{Scheme: "socks5", Host: addr, User: url.UserPassword("u", "wrong")}
	if _, err := dialSOCKS5(context.Background(), bad, "example.com:443"); err == nil {
		t.Fatal("错误密码应失败")
	}
}

func TestParseProxyStatus(t *testing.T) {
	cases := []struct {
		line string
		code int
		ok   bool
	}{
		{"HTTP/1.1 200 Connection established\r\n", 200, true},
		{"HTTP/1.0 407 Proxy Authentication Required\r\n", 407, true},
		{"garbage\r\n", 0, false},
	}
	for _, c := range cases {
		code, err := parseProxyStatus(c.line)
		if c.ok && (err != nil || code != c.code) {
			t.Fatalf("%q → code=%d err=%v", c.line, code, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q 应解析失败", c.line)
		}
	}
}

func TestProxyBasicAuthHeaderEncoding(t *testing.T) {
	// 只是确认 CONNECT 请求里 Basic 头的编码形态（u:p → dTpw）
	if got := base64.StdEncoding.EncodeToString([]byte("u:p")); got != "dTpw" {
		t.Fatalf("base64 编码异常：%s", got)
	}
}
