package main

// Claude Desktop「chat + code 同时」的本地分流代理（路 A：HTTPS_PROXY + 本地 CA）。
//
// 背景（2026-11-09）：Claude Desktop 的 3P 模式会把整个 app 切成第三方部署形态
// —— chat 走网关、claude.ai 的会话/连接器/手机远程全关（见 asar 的
// hasClaudeAiProductFeatures(){return !1}）。用户要的是「chat 还是官方账号的，
// code 用 aihub 的模型」。
//
// 官方没这个开关，能落地的只有一条：让 app 保持 1P 登录态，只把 Code 子进程
// 的流量引到本地代理上。Desktop 给 Code 子进程留的入口正好是 Anthropic 文档
// 里企业代理那套（HTTPS_PROXY + NODE_EXTRA_CA_CERTS，两者都在 asar 的 env
// 白名单 dqe 里）。
//
// 分流规则（按请求体 model 字段）：
//   - claude-* 且不是聚合目录里的条目 → 原样透传 api.anthropic.com（字节不改）
//   - 其余（dom./qoder./trae. 等）      → 改写 base 到 :8790 聚合口
//
// 只在 mitmTargetHosts 列出的域名上终止 TLS；其他 CONNECT 一律纯隧道，不解密。

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// mitmPort 是本地分流代理的监听端口。挑 18790 是因为 18788 已被占用、
	// 18791 留给 clauderipple 风格的远端隧道，避开常用区间。
	mitmPort = 18790

	// mitmPrimaryHost 是 TLS 客户端没发 SNI 时用的兜底主机名。
	mitmPrimaryHost = "api.anthropic.com"

	// mitmMaxBody 单请求体上限。Claude Code 带图/长上下文时能到几十 MB，
	// 给到 64MB 与聚合口一致。
	mitmMaxBody = 64 << 20

	mitmDialTimeout  = 15 * time.Second
	mitmReadTimeout  = 10 * time.Minute
	mitmShutdownWait = 3 * time.Second
)

// mitmTargetHosts 是需要解密的域名。只列 Anthropic API：Code 子进程只打这里。
// 任何其他域名（含 claude.ai 本体、CDN、遥测）一律纯隧道，不看内容。
var mitmTargetHosts = map[string]bool{
	"api.anthropic.com": true,
}

// mitmProxy 是分流代理。Start/Stop 与线路同一套语义（幂等、可重复启停）。
type mitmProxy struct {
	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	ca      *mitmCA
	started time.Time

	reqTotal  atomic.Int64
	reqLocal  atomic.Int64
	reqPass   atomic.Int64
	reqTunnel atomic.Int64

	// upstreamOverride / aggOverride 只给测试用：把「真 api.anthropic.com」
	// 和「真聚合口」换成 127.0.0.1 上的假服务，这样端到端测试不用出网、
	// 不碰凭据。生产路径留空即用真实地址。
	upstreamOverride string
	aggOverride      string
	upstreamClient   *http.Client // 测试用：指向假上游、信任测试 CA
	localClient      *http.Client // 测试用：指向假聚合口
}

func newMITMProxy(ca *mitmCA) *mitmProxy {
	return &mitmProxy{ca: ca}
}

func (p *mitmProxy) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ln != nil
}

// Start 绑定端口并起服务。与线路一致：失败先清端口再重试几次。
func (p *mitmProxy) Start() (bool, string) {
	if p.Running() {
		return true, "已在运行"
	}
	if p.ca == nil {
		return false, "本地 CA 未就绪"
	}
	ln, err := listenLinePort(mitmPort)
	if err != nil {
		return false, fmt.Sprintf("端口 %d 被占用，无法启动：%v", mitmPort, err)
	}
	srv := newLineServer(http.HandlerFunc(p.serveHTTP))
	srv.ReadTimeout = 0 // 长连接 + 长流式响应，交给各请求自己的 deadline
	p.mu.Lock()
	p.ln = ln
	p.srv = srv
	p.started = time.Now()
	p.mu.Unlock()
	go func() {
		defer recoverGoroutine("mitm_proxy.go:Serve")
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			logHub("MITM 代理退出：%v", serr)
		}
	}()
	logHub("MITM 分流代理已启动：127.0.0.1:%d（CA %s）", mitmPort, p.ca.fingerprint()[:23])
	return true, fmt.Sprintf("已启动（127.0.0.1:%d）", mitmPort)
}

func (p *mitmProxy) Stop() (bool, string) {
	p.mu.Lock()
	srv, ln := p.srv, p.ln
	p.srv, p.ln = nil, nil
	p.mu.Unlock()
	if srv == nil && ln == nil {
		return true, "未在运行"
	}
	closeLineServer(srv, ln)
	if waitPortFree(mitmPort, mitmShutdownWait) {
		return true, "已停止"
	}
	return false, "已发出停止，但端口仍被占用"
}

// serveHTTP 处理两种形态：
//   - CONNECT host:port —— 浏览器/CLI 走代理时的隧道请求
//   - 其余 —— 走 http:// 的普通代理请求（Anthropic 全是 https，这里只兜底）
func (p *mitmProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "AIHub MITM proxy: 只接受 CONNECT", http.StatusMethodNotAllowed)
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 Hijack", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	p.reqTotal.Add(1)

	if !mitmTargetHosts[strings.ToLower(host)] {
		p.reqTunnel.Add(1)
		go p.tunnel(conn, rw, r.Host)
		return
	}
	go p.mitmConn(conn, rw, host)
}

// tunnel 纯转发（不解密）。用于所有非目标域名。
func (p *mitmProxy) tunnel(conn net.Conn, rw *bufio.ReadWriter, addr string) {
	defer conn.Close()
	up, err := net.DialTimeout("tcp", addr, mitmDialTimeout)
	if err != nil {
		logHub("MITM 隧道连接 %s 失败：%v", addr, err)
		return
	}
	defer up.Close()
	if n := rw.Reader.Buffered(); n > 0 {
		if _, err := io.CopyN(up, rw, int64(n)); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, up); done <- struct{}{} }()
	<-done
}

// mitmConn 在已建立的隧道里做 TLS 终止，然后按 HTTP 请求分流。
func (p *mitmProxy) mitmConn(conn net.Conn, rw *bufio.ReadWriter, host string) {
	defer conn.Close()
	// 客户端可能已经把 ClientHello 发过来了（rw 里还有缓冲），
	// 直接复用 hijack 得到的 bufio.Reader 继续读，避免丢首包。
	var raw net.Conn = conn
	if rw.Reader.Buffered() > 0 {
		raw = &bufferedConn{Conn: conn, r: rw.Reader}
	}
	tlsConn := tls.Server(raw, &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return p.ca.leafFor(name)
		},
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"}, // 不协商 h2：分流逻辑按 HTTP/1.1 写，简单可靠
	})
	_ = tlsConn.SetDeadline(time.Now().Add(mitmDialTimeout))
	if err := tlsConn.Handshake(); err != nil {
		logHub("MITM TLS 握手失败（%s）：%v", host, err)
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		req.URL.Scheme = "https"
		req.URL.Host = host
		keepAlive := p.handleOne(tlsConn, req, host)
		_ = req.Body.Close()
		if !keepAlive {
			return
		}
	}
}

// mitmLocalModel 判断这个模型名是否该走聚合口。
//
// 与 resolveAggThirdParty 的关键差别：**不认 Claude Desktop 的角色名**
// （claude-sonnet-4-6 / claude-opus-5 之类）。那些是 3P 模式的槽位名，
// 只有在 3P 配置里才有意义；1P 下客户端发来的是 Anthropic 的真实模型名，
// 必须原样透传上游 —— 否则「chat 走官方」这条就断了。
//
// 认的形式只有聚合口自己的三种：白名单里的完整 slug、ccs-线路/模型、线路.模型。
func mitmLocalModel(slug string) bool {
	if slug == "" {
		return false
	}
	// 兜住所有 Anthropic 原生模型名（含带日期后缀的）。
	if strings.HasPrefix(strings.ToLower(slug), "claude-") {
		return false
	}
	// gpt-* 是 Codex 官方的，也不该被 MITM 这条（Code 走 Anthropic 协议）接走。
	if isOfficialModelSlug(slug) {
		return false
	}
	_, ok := resolveAggThirdParty(slug)
	return ok
}

// handleOne 处理一个已解密的请求，返回是否继续复用连接。
func (p *mitmProxy) handleOne(w io.Writer, req *http.Request, host string) bool {
	body, err := io.ReadAll(io.LimitReader(req.Body, mitmMaxBody))
	if err != nil {
		writeRawResponse(w, http.StatusBadRequest, "text/plain", []byte("read body: "+err.Error()))
		return false
	}
	model := requestedModelSlug(body)

	if mitmLocalModel(model) {
		p.reqLocal.Add(1)
		return p.forwardLocal(w, req, body, model)
	}
	// 不是聚合目录里的模型 → 原样透传官方（字节不改，与客户端自己发的完全一致）。
	p.reqPass.Add(1)
	return p.forwardUpstream(w, req, body, host)
}

// forwardLocal 把请求改道到聚合口。baseURL 由模型所属线路决定：
// 走 /v1/messages（Anthropic 协议），聚合口内部再转 chat。
func (p *mitmProxy) forwardLocal(w io.Writer, req *http.Request, body []byte, model string) bool {
	base := fmt.Sprintf("127.0.0.1:%d", aggPort)
	if p.aggOverride != "" {
		base = p.aggOverride
	}
	target := "http://" + base + "/v1/messages"
	hr, err := http.NewRequestWithContext(req.Context(), req.Method, target, bytes.NewReader(body))
	if err != nil {
		writeRawResponse(w, http.StatusBadGateway, "text/plain", []byte(err.Error()))
		return false
	}
	copyProxyHeaders(hr.Header, req.Header)
	hr.Header.Del("Host")
	hr.ContentLength = int64(len(body))

	// 打本地聚合口：复用 aggHTTPClient（与 proxy_anthropic.go 同一套）。
	client := aggHTTPClient()
	if p.localClient != nil {
		client = p.localClient
	}
	t0 := time.Now()
	resp, err := client.Do(hr)
	if err != nil {
		logHub("MITM 转发聚合口失败（%s）：%v", model, err)
		writeRawResponse(w, http.StatusBadGateway, "text/plain", []byte("聚合口连接失败："+err.Error()))
		return false
	}
	defer resp.Body.Close()
	ok := writeUpstreamResponse(w, resp, req, false)
	logHub("MITM → 聚合口 %s | %s | %d | %s", model, req.URL.Path, resp.StatusCode, time.Since(t0).Round(time.Millisecond))
	return ok
}

// forwardUpstream 原样透传到真 Anthropic：头、体、路径都不动，
// 只把 Host 指向目标域名（证书是本地签的，客户端不校验上游）。
func (p *mitmProxy) forwardUpstream(w io.Writer, req *http.Request, body []byte, host string) bool {
	dialHost := host
	if p.upstreamOverride != "" {
		dialHost = p.upstreamOverride
	}
	target := "https://" + dialHost + req.URL.RequestURI()
	hr, err := http.NewRequestWithContext(req.Context(), req.Method, target, bytes.NewReader(body))
	if err != nil {
		writeRawResponse(w, http.StatusBadGateway, "text/plain", []byte(err.Error()))
		return false
	}
	copyProxyHeaders(hr.Header, req.Header)
	hr.Host = host
	hr.ContentLength = int64(len(body))

	client := mitmUpstreamClient()
	if p.upstreamClient != nil {
		client = p.upstreamClient
	}
	resp, err := client.Do(hr)
	if err != nil {
		logHub("MITM 透传 %s 失败：%v", host, err)
		writeRawResponse(w, http.StatusBadGateway, "text/plain", []byte("上游连接失败："+err.Error()))
		return false
	}
	defer resp.Body.Close()
	return writeUpstreamResponse(w, resp, req, true)
}

// writeUpstreamResponse 把上游响应写回客户端连接。streaming 时逐块 flush，
// 让 SSE 立刻到客户端（Code 的流式体验靠这个）。
func writeUpstreamResponse(w io.Writer, resp *http.Response, req *http.Request, streaming bool) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	if streaming {
		b.WriteString("Transfer-Encoding: chunked\r\n")
	} else if resp.ContentLength >= 0 {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", resp.ContentLength)
	} else {
		b.WriteString("Transfer-Encoding: chunked\r\n")
	}
	if req.Close {
		b.WriteString("Connection: close\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return false
	}
	if req.Method == http.MethodHead {
		return true
	}
	if f, ok := w.(http.Flusher); ok {
		_ = f
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if streaming {
				if _, werr := fmt.Fprintf(w, "%x\r\n", n); werr != nil {
					return false
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return false
			}
			if streaming {
				if _, werr := io.WriteString(w, "\r\n"); werr != nil {
					return false
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if streaming {
		_, _ = io.WriteString(w, "0\r\n\r\n")
	}
	return true
}

func writeRawResponse(w io.Writer, status int, ctype string, body []byte) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		status, http.StatusText(status), ctype, len(body))
	_, _ = w.Write(body)
}

// copyProxyHeaders 复制客户端原始头（逐字保留，不增删）。
func copyProxyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if strings.EqualFold(k, "Proxy-Connection") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

var (
	mitmUpOnce sync.Once
	mitmUpVal  *http.Client
)

// mitmUpstreamClient 访问真 Anthropic：不走环境代理（否则会绕回自己），
// 也不校验证书链（我们只做转发，不做安全判断）。
func mitmUpstreamClient() *http.Client {
	mitmUpOnce.Do(func() {
		tr := &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   mitmDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		mitmUpVal = &http.Client{Transport: tr, Timeout: 0}
	})
	return mitmUpVal
}

// mitmStatus 给控制台/API 用的快照。
type mitmStatus struct {
	Running   bool   `json:"running"`
	Port      int    `json:"port"`
	ProxyURL  string `json:"proxy_url"`
	CAPath    string `json:"ca_path"`
	CAFinger  string `json:"ca_fingerprint"`
	CAExists  bool   `json:"ca_exists"`
	Requests  int64  `json:"requests"`
	Local     int64  `json:"routed_local"`
	Passthru  int64  `json:"routed_passthrough"`
	Tunneled  int64  `json:"tunneled"`
	StartedAt string `json:"started_at,omitempty"`
}

func (p *mitmProxy) status() mitmStatus {
	p.mu.Lock()
	running := p.ln != nil
	startedAt := ""
	if running && !p.started.IsZero() {
		startedAt = p.started.Format("2006-01-02 15:04:05")
	}
	p.mu.Unlock()
	st := mitmStatus{
		Running:   running,
		Port:      mitmPort,
		ProxyURL:  fmt.Sprintf("http://127.0.0.1:%d", mitmPort),
		CAPath:    mitmCACertPath(),
		Requests:  p.reqTotal.Load(),
		Local:     p.reqLocal.Load(),
		Passthru:  p.reqPass.Load(),
		Tunneled:  p.reqTunnel.Load(),
		StartedAt: startedAt,
	}
	if p.ca != nil {
		st.CAExists = true
		st.CAFinger = p.ca.fingerprint()
	}
	return st
}

// mitmEnvSnippet 是给用户复制到 shell 的两行（Code 子进程继承这两项）。
func mitmEnvSnippet() string {
	return fmt.Sprintf("export HTTPS_PROXY=http://127.0.0.1:%d\nexport NODE_EXTRA_CA_CERTS=%s\n",
		mitmPort, mitmCACertPath())
}
