package main

// 出站 TLS 指纹伪装（uTLS，2026-11-05 用户要求「加 TLS 指纹伪装」）。
//
// 要解决的问题：hub 用 Go 自己的 crypto/tls 打上游，JA3/JA4 是「Go 客户端」的形状
// （固定扩展顺序、没有 GREASE、TLS1.3 只带少数套件、H2 SETTINGS 也是 Go 的写法）。
// 上游 2026-10-23 起把「请求指纹 + 身份句 + 请求节奏」当组合判据（见
// docs/2026-10-25-封号风险与出站身份对齐.md），身份头和节奏已经在前面几轮对齐了，
// 唯独 TLS 这一层还是裸的 —— 本文件把它补上。
//
// 做法：只在**拨号层**替换 TLS 握手，HTTP 语义完全不动。
//   - 每个线路一个固定的 ClientHelloID（固定才好，真实客户端不会每次换指纹）；
//   - 复用 utls 的 Chrome 133 / Firefox 120 / Safari 16 / Edge 85 / Go 默认五档；
//   - Electron 系（WorkBuddy / Qoder / Trae / Zcode / TeleAgent）默认 chrome；
//     opencode 官方只有 CLI（Node/undici），保持 golang 档，别硬装浏览器；
//   - 代理要自己做（net/http 的 DialTLSContext 在「走代理」时根本不被调用，
//     见 /usr/local/go/src/net/http/transport.go:1885 的 addTLS 分支），
//     所以这里接管 Proxy：HTTP CONNECT / SOCKS5 隧道都自己建，再在隧道里做 uTLS。
//
// 开关（环境变量，改完重启控制台生效）：
//
//	AIHUB_TLS_FP=chrome|firefox|safari|edge|golang|off   全局默认（默认 chrome）
//	AIHUB_TLS_FP_INTL=firefox                            按线路覆盖（线路名大写）
//	AIHUB_TLS_FP_FALLBACK=0                              关掉「握手失败回退 Go TLS」
//	AIHUB_TLS_FP_DEBUG=1                                 每次握手打一行日志
//
// 注意：本文件只改「出站到上游」的 TLS；本地监听（127.0.0.1:87xx/8790）仍是明文
// HTTP，客户端（DSH / Codex / 浏览器）那边不需要证书。

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

const (
	tlsFPEnvGlobal   = "AIHUB_TLS_FP"
	tlsFPEnvLinePfx  = "AIHUB_TLS_FP_"
	tlsFPEnvFallback = "AIHUB_TLS_FP_FALLBACK"
	tlsFPEnvDebug    = "AIHUB_TLS_FP_DEBUG"

	// 与 proxy_util.go 里原来的 TLSHandshakeTimeout 保持一致：
	// 自定义 DialTLSContext 下 net/http 不再管握手超时，得自己控。
	tlsFPHandshakeTimeout = 15 * time.Second
)

// tlsFPSpec 一个可选指纹档。
type tlsFPSpec struct {
	Key   string
	Hello utls.ClientHelloID
	Note  string
}

// tlsFPProfiles 档位表。键就是环境变量里写的值。
var tlsFPProfiles = map[string]tlsFPSpec{
	"chrome":  {Key: "chrome", Hello: utls.HelloChrome_Auto, Note: "Chrome 133（Electron 桌面端同族）"},
	"firefox": {Key: "firefox", Hello: utls.HelloFirefox_Auto, Note: "Firefox 120"},
	"safari":  {Key: "safari", Hello: utls.HelloSafari_Auto, Note: "Safari 16"},
	"edge":    {Key: "edge", Hello: utls.HelloEdge_Auto, Note: "Edge 85"},
	"golang":  {Key: "golang", Hello: utls.HelloGolang, Note: "Go 默认指纹（CLI 线路用）"},
}

// tlsFPDefaultProfile 线路的默认档：来自客户端画像（client_profile.go）。
// opencode 官方只有 CLI，保持 Go/CLI 形状；其余都是 Electron 桌面端，用 chrome。
func tlsFPDefaultProfile(line string) string {
	if p := clientProfileFor(line); p.TLS != "" {
		return p.TLS
	}
	return "chrome"
}

// tlsFPProfileFor 解析某条线路该用哪档指纹。
// ok=false 表示关闭伪装（走原来的 crypto/tls 路径）。
func tlsFPProfileFor(line string) (tlsFPSpec, bool) {
	raw := ""
	if line != "" {
		raw = strings.TrimSpace(os.Getenv(tlsFPEnvLinePfx + strings.ToUpper(strings.ReplaceAll(line, "-", "_"))))
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv(tlsFPEnvGlobal))
	}
	if raw == "" {
		raw = tlsFPDefaultProfile(line)
	}
	switch strings.ToLower(raw) {
	case "off", "0", "false", "no", "disable", "disabled", "none":
		return tlsFPSpec{}, false
	}
	if spec, ok := tlsFPProfiles[strings.ToLower(raw)]; ok {
		return spec, true
	}
	// 写错了不静默降级成 off：按默认档走，并在日志里说清楚（否则「以为开了其实没开」）。
	logHub("AIHUB_TLS_FP=%q 不是已知档位（chrome/firefox/safari/edge/golang/off），按 %s 处理", raw, tlsFPDefaultProfile(line))
	return tlsFPProfiles[tlsFPDefaultProfile(line)], true
}

func tlsFPFallbackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(tlsFPEnvFallback))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func tlsFPDebug() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(tlsFPEnvDebug))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ---------- 运行统计（给 /api/tls-fingerprint 和日志用） ----------

type tlsFPLineStat struct {
	Profile     string    `json:"profile"`
	Handshakes  int       `json:"handshakes"`
	Fallbacks   int       `json:"fallbacks"`
	Failures    int       `json:"failures"`
	LastALPN    string    `json:"last_alpn"`
	LastVersion string    `json:"last_tls"`
	LastAt      time.Time `json:"last_at"`
	LastErr     string    `json:"last_err,omitempty"`
}

var (
	tlsFPStatsMu sync.Mutex
	tlsFPStats   = map[string]*tlsFPLineStat{}
)

func tlsFPRecord(line string, profile string, st tls.ConnectionState, err error) {
	key := line
	if key == "" {
		key = "(default)"
	}
	tlsFPStatsMu.Lock()
	s := tlsFPStats[key]
	if s == nil {
		s = &tlsFPLineStat{Profile: profile}
		tlsFPStats[key] = s
	}
	s.Profile = profile
	s.LastAt = time.Now()
	if err != nil {
		s.Failures++
		s.LastErr = truncate(err.Error(), 160)
	} else {
		s.Handshakes++
		s.LastALPN = st.NegotiatedProtocol
		s.LastVersion = tlsVersionName(st.Version)
		s.LastErr = ""
	}
	tlsFPStatsMu.Unlock()
}

func tlsFPRecordFallback(line string) {
	key := line
	if key == "" {
		key = "(default)"
	}
	tlsFPStatsMu.Lock()
	if s := tlsFPStats[key]; s != nil {
		s.Fallbacks++
	}
	tlsFPStatsMu.Unlock()
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	}
	return "0x" + strconv.FormatUint(uint64(v), 16)
}

// tlsFingerprintSummary 给控制台/接口的只读视图。
func tlsFingerprintSummary() map[string]any {
	lines := map[string]any{}
	enabled := false
	tlsFPStatsMu.Lock()
	for k, v := range tlsFPStats {
		cp := *v
		lines[k] = cp
	}
	tlsFPStatsMu.Unlock()
	for _, spec := range lineSpecs {
		if _, ok := tlsFPProfileFor(spec.Key); ok {
			enabled = true
		}
	}
	return map[string]any{
		"enabled":  enabled,
		"global":   tlsFPGlobalLabel(),
		"fallback": tlsFPFallbackEnabled(),
		"lines":    lines,
		"profiles": tlsFPProfileNames(),
		"clients":  clientProfilesSummary(),
	}
}

func tlsFPGlobalLabel() string {
	raw := strings.TrimSpace(os.Getenv(tlsFPEnvGlobal))
	if raw == "" {
		return "默认：桌面端 chrome / CLI golang"
	}
	return raw
}

func tlsFPProfileNames() []string {
	out := make([]string, 0, len(tlsFPProfiles))
	for k := range tlsFPProfiles {
		out = append(out, k)
	}
	return out
}

// ---------- transport 接线 ----------

var tlsFPSessionCache = utls.NewLRUClientSessionCache(128)

// tlsFPTestConfigHook 仅供测试注入（生产恒为 nil）。
// 用来给本地 httptest 自签证书塞 RootCAs —— 正式路径的证书校验完全不动。
// 用函数而不是直接存 *utls.Config：utls.Config 内含 RWMutex，按值复制会被 vet 拦。
var tlsFPTestConfigHook func(host string) *utls.Config

var tlsFPDialerBase = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}

// tlsFPDialer 一条线路的拨号器：代理自己建，TLS 用指定 ClientHello。
type tlsFPDialer struct {
	line     string
	profile  string
	spec     tlsFPSpec
	useProxy bool
	// h2=false 时：ClientHello 的 ALPN 收窄成只有 http/1.1，transport 也真禁 h2。
	// 这是「装得像客户端」的一部分：见 client_profile.go 文件头的社区依据。
	h2 bool
}

// applyTLSFingerprint 把 transport 的拨号层换成 uTLS。
// 返回 true 表示已接管；false = 关着指纹伪装，transport 保持原样。
func applyTLSFingerprint(tr *http.Transport, line string) bool {
	spec, ok := tlsFPProfileFor(line)
	if !ok {
		return false
	}
	cp := clientProfileFor(line)
	d := &tlsFPDialer{line: line, profile: spec.Key, spec: spec, useProxy: tr.Proxy != nil, h2: cp.HTTP2}
	// 关键：必须把 net/http 的 Proxy 关掉。
	// 走代理时 net/http 会先在 addTLS 里判断 cm.scheme()=="http"（代理的 scheme），
	// 从而绕开自定义 DialTLSContext，改用 crypto/tls 自己握手 —— 指纹又回到 Go。
	// 所以代理判断、CONNECT、隧道内的 TLS 全由本文件接管。
	tr.Proxy = nil
	tr.DialContext = d.dialContext
	tr.DialTLSContext = d.dialTLSContext
	tr.ForceAttemptHTTP2 = d.h2
	if !d.h2 {
		// 非 nil 的空 map 是标准库文档里「关掉 HTTP/2」的写法（transport.go:254）。
		// 光把 ForceAttemptHTTP2 设 false 不够：默认 TLS 路径仍会经 ALPN 协商 h2，
		// 然后拿 Go 的 h2 帧去对话，指纹反而更矛盾（社区实证见 client_profile.go）。
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return true
}

// dialContext 只服务明文 HTTP（仓库里所有上游都是 https，这里只为兼容本地/异常情况）。
// 明文请求没法「先 CONNECT 再发绝对 URI」（net/http 只在自己管代理时才那样拼请求），
// 所以这里直连，不套代理。
func (d *tlsFPDialer) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return tlsFPDialerBase.DialContext(ctx, network, addr)
}

// dialTLSContext net/http 在「非代理 https」路径调用它；本文件把 Proxy 关掉后，
// 所有 https（含需要走代理的）都会走到这里。
func (d *tlsFPDialer) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, proxy, err := d.dialRaw(ctx, addr)
	if err != nil {
		tlsFPRecord(d.line, d.profile, tls.ConnectionState{}, err)
		return nil, err
	}
	conn, st, err := d.handshake(ctx, raw, addr)
	if err == nil {
		tlsFPRecord(d.line, d.profile, st, nil)
		if tlsFPDebug() {
			logHub("[tlsfp] %s → %s 握手完成 profile=%s alpn=%q tls=%s 代理=%v",
				d.line, addr, d.profile, st.NegotiatedProtocol, tlsVersionName(st.Version), proxyLabel(proxy))
		}
		return conn, nil
	}
	_ = raw.Close()
	tlsFPRecord(d.line, d.profile, tls.ConnectionState{}, err)

	// 回退：uTLS 握手失败（上游只认某类扩展、或中间设备干扰）时，用 Go 自带 TLS
	// 再拨一次，保证线路可用。证书错误不回退 —— Go 也一样过不了，重拨只会掩盖问题。
	if !tlsFPFallbackEnabled() || isCertVerifyError(err) {
		return nil, err
	}
	tlsFPRecordFallback(d.line)
	logHub("[tlsfp] %s → %s uTLS(%s) 握手失败，回退 Go TLS：%v", d.line, addr, d.profile, err)
	raw2, _, err2 := d.dialRaw(ctx, addr)
	if err2 != nil {
		return nil, err2
	}
	std, err3 := handshakeGoTLS(ctx, raw2, addr)
	if err3 != nil {
		_ = raw2.Close()
		return nil, err3
	}
	return std, nil
}

// dialRaw 建一条到目标的原始 TCP（需要时先穿过代理隧道）。
func (d *tlsFPDialer) dialRaw(ctx context.Context, addr string) (net.Conn, *url.URL, error) {
	var proxy *url.URL
	if d.useProxy {
		proxy = proxyForTarget(addr)
	}
	if proxy == nil {
		c, err := tlsFPDialerBase.DialContext(ctx, "tcp", addr)
		return c, nil, err
	}
	c, err := dialViaProxy(ctx, proxy, addr)
	return c, proxy, err
}

func proxyLabel(p *url.URL) string {
	if p == nil {
		return "直连"
	}
	return p.Scheme + "://" + p.Host
}

// handshake 在已建立的 TCP 连接上做 uTLS 握手，返回包装过的连接与协商结果。
func (d *tlsFPDialer) handshake(ctx context.Context, raw net.Conn, addr string) (net.Conn, tls.ConnectionState, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := &utls.Config{
		MinVersion:         tls.VersionTLS12,
		ClientSessionCache: tlsFPSessionCache,
		// HelloCustom 的 spec（我们收窄过 ALPN 的那份）里没有 PSK 扩展，而 session
		// cache 命中的会话会让 utls 尝试续用 —— 没有 PSK 扩展时它会 **panic**
		// （tls: initPskExt failed，2026-11-05 全量测试实测）。让它在缺扩展时
		// 安静地跳过续用即可，握手照常走完整流程。
		PreferSkipResumptionOnNilExtension: true,
	}
	if net.ParseIP(host) == nil {
		cfg.ServerName = host // IP 不发 SNI（浏览器也不发）
	}
	if tlsFPTestConfigHook != nil {
		cfg = tlsFPTestConfigHook(host)
	}
	var uconn *utls.UConn
	if !d.h2 && d.spec.Key != "golang" {
		// 把 ALPN 收窄成只有 http/1.1：既保证不协商出 h2，也让 ClientHello 的
		// ALPN 段与「不发 h2 的客户端」自洽。golang 档不发 ALPN（Go 客户端本来也不发），
		// 硬加一个反而多一处不像。
		spec, err := tlsFPSpecWithALPN(d.spec, []string{"http/1.1"})
		if err != nil {
			return nil, tls.ConnectionState{}, fmt.Errorf("收窄 %s 的 ALPN 失败：%w", d.spec.Key, err)
		}
		// utls 的「协商判定」用的是 config.NextProtos，而 spec 里的 ALPN 扩展只决定
		// 线上字节；两边都设成 http/1.1，才不会出现「字节里只有一个协议、判定表是空的」
		// （实测后果：ALPN 协商结果为空，h2 又被 transport 关掉，请求走到半路才炸）。
		cfg.NextProtos = []string{"http/1.1"}
		uconn = utls.UClient(raw, cfg, utls.HelloCustom)
		if err := uconn.ApplyPreset(spec); err != nil {
			return nil, tls.ConnectionState{}, fmt.Errorf("构造 %s 的 http/1.1 spec 失败：%w", d.spec.Key, err)
		}
	} else {
		uconn = utls.UClient(raw, cfg, d.spec.Hello)
	}
	hctx, cancel := context.WithTimeout(ctx, tlsFPHandshakeTimeout)
	defer cancel()
	if err := uconn.HandshakeContext(hctx); err != nil {
		return nil, tls.ConnectionState{}, err
	}
	st := uconn.ConnectionState()
	return &utlsTLSConn{conn: uconn, state: toStdTLSState(st)}, toStdTLSState(st), nil
}

// tlsFPSpecWithALPN 把某个档位的 ClientHello 克隆一份，并把 ALPN 换成指定列表。
//
// 三个必须注意的点（2026-11-05 社区调研，misakano7545/traework2api-panel 踩过）：
//  1. 每次返回**新拷贝**：utls 握手时会改写 spec（GREASE 值、key share 长度），
//     全局共享同一份会串味；
//  2. ALPN 要用新的扩展对象，不能复用共享切片；
//  3. 有状态扩展（GREASE / Padding / KeyShare）必须重建：GREASE 会把自己抽到的
//     value 写回对象，Padding 会缓存算出的长度，多连接共用一个实例会随机
//     `tls: internal error`。
func tlsFPSpecWithALPN(fp tlsFPSpec, alpn []string) (*utls.ClientHelloSpec, error) {
	base, err := utls.UTLSIdToSpec(fp.Hello)
	if err != nil {
		return nil, err
	}
	spec := &utls.ClientHelloSpec{
		TLSVersMin:         base.TLSVersMin,
		TLSVersMax:         base.TLSVersMax,
		CipherSuites:       append([]uint16(nil), base.CipherSuites...),
		CompressionMethods: append([]byte(nil), base.CompressionMethods...),
		Extensions:         append([]utls.TLSExtension(nil), base.Extensions...),
		GetSessionID:       base.GetSessionID,
	}
	found := false
	for i, e := range spec.Extensions {
		switch e.(type) {
		case *utls.ALPNExtension:
			spec.Extensions[i] = &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)}
			found = true
		}
	}
	if !found && len(alpn) > 0 {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
	}
	// 注意：这里**不重建**有状态扩展。UTLSIdToSpec 每次返回的都是新对象
	// （GREASE 值、padding 长度、key share 密钥都在握手时现生成），重建反而会
	// 造出结构不完整的 ClientHello（实测：重建后服务端回 "error decoding message"）。
	// 只有当 spec 是「手工复刻并长期持有」时才需要重建 —— 见社区踩坑记录。
	return spec, nil
}

// handshakeGoTLS 回退路径：标准库 TLS（指纹就是 Go 的，属于应急）。
func handshakeGoTLS(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if net.ParseIP(host) != nil {
		cfg.ServerName = ""
	}
	tc := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, tlsFPHandshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		return nil, err
	}
	return tc, nil
}

func isCertVerifyError(err error) bool {
	var unknownAuth x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &unknownAuth) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

// utlsTLSConn 把 uTLS 连接适配成 net/http 想要的样子。
//
// 为什么必须适配：net/http 判断「这条连接能不能跑 HTTP/2」靠的是
// `interface{ ConnectionState() crypto/tls.ConnectionState }`
// （transport.go:1892）。utls.Conn.ConnectionState() 返回的是 utls 自己的
// ConnectionState 类型，类型不匹配 → net/http 看不到 ALPN，HTTP/2 静默降级成 1.1。
// 这里做一次字段搬运，并把结果缓存下来。
type utlsTLSConn struct {
	conn  *utls.UConn
	state tls.ConnectionState
}

func (c *utlsTLSConn) Read(b []byte) (int, error)         { return c.conn.Read(b) }
func (c *utlsTLSConn) Write(b []byte) (int, error)        { return c.conn.Write(b) }
func (c *utlsTLSConn) Close() error                       { return c.conn.Close() }
func (c *utlsTLSConn) LocalAddr() net.Addr                { return c.conn.LocalAddr() }
func (c *utlsTLSConn) RemoteAddr() net.Addr               { return c.conn.RemoteAddr() }
func (c *utlsTLSConn) SetDeadline(t time.Time) error      { return c.conn.SetDeadline(t) }
func (c *utlsTLSConn) SetReadDeadline(t time.Time) error  { return c.conn.SetReadDeadline(t) }
func (c *utlsTLSConn) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

// ConnectionState 满足 net/http 的 connectionStater 接口（返回标准库类型）。
func (c *utlsTLSConn) ConnectionState() tls.ConnectionState { return c.state }

// toStdTLSState 把 utls.ConnectionState 搬成 crypto/tls.ConnectionState。
// 只搬两边都有的字段；Go 新增的字段留零值，不影响 net/http 的判断。
func toStdTLSState(s utls.ConnectionState) tls.ConnectionState {
	return tls.ConnectionState{
		Version:                     s.Version,
		HandshakeComplete:           s.HandshakeComplete,
		DidResume:                   s.DidResume,
		CipherSuite:                 s.CipherSuite,
		NegotiatedProtocol:          s.NegotiatedProtocol,
		NegotiatedProtocolIsMutual:  s.NegotiatedProtocolIsMutual,
		ServerName:                  s.ServerName,
		PeerCertificates:            s.PeerCertificates,
		VerifiedChains:              s.VerifiedChains,
		SignedCertificateTimestamps: s.SignedCertificateTimestamps,
		OCSPResponse:                s.OCSPResponse,
		TLSUnique:                   s.TLSUnique,
	}
}

// ---------- 代理 ----------

// proxyForTarget 复刻 newBackendClient 原本的代理选择逻辑：
// 环境变量优先 → 探活 → 不通就直连；没有环境变量时看系统代理。
func proxyForTarget(addr string) *url.URL {
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: addr}}
	return resolveRequestProxy(req)
}

func dialViaProxy(ctx context.Context, p *url.URL, target string) (net.Conn, error) {
	switch strings.ToLower(p.Scheme) {
	case "http", "https":
		return dialHTTPProxy(ctx, p, target)
	case "socks5", "socks5h":
		return dialSOCKS5(ctx, p, target)
	}
	return nil, fmt.Errorf("不支持的代理协议 %q（只认 http/https/socks5）", p.Scheme)
}

// dialHTTPProxy 连代理 → （https 代理先和代理握手）→ CONNECT target → 返回隧道。
func dialHTTPProxy(ctx context.Context, p *url.URL, target string) (net.Conn, error) {
	addr := p.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		port := "80"
		if strings.EqualFold(p.Scheme, "https") {
			port = "443"
		}
		addr = net.JoinHostPort(p.Host, port)
	}
	conn, err := tlsFPDialerBase.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连代理 %s 失败：%w", addr, err)
	}
	if strings.EqualFold(p.Scheme, "https") {
		host, _, _ := net.SplitHostPort(addr)
		tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		hctx, cancel := context.WithTimeout(ctx, tlsFPHandshakeTimeout)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("和 https 代理 %s 握手失败：%w", addr, err)
		}
		conn = tc
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n", target, target)
	if p.User != nil {
		pass, _ := p.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(p.User.Username() + ":" + pass))
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", token)
	}
	b.WriteString("\r\n")

	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 发送失败：%w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("代理没有回 CONNECT 响应：%w", err)
	}
	// 读完响应头（到空行为止）。Header 内容对 CONNECT 无意义，只要状态码。
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理 CONNECT 响应被截断：%w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	code, err := parseProxyStatus(status)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if code != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("代理 CONNECT %s 返回 %d", target, code)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

func parseProxyStatus(line string) (int, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, fmt.Errorf("代理返回了非 HTTP 响应：%q", truncate(line, 80))
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("代理状态码无法解析：%q", truncate(line, 80))
	}
	return code, nil
}

// bufferedConn 让 CONNECT 之后残留的缓冲字节不丢（bufio 可能已经读进了 TLS 首包）。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// dialSOCKS5 最小 SOCKS5 客户端（无认证 / 用户名密码认证都支持，远端解析域名）。
func dialSOCKS5(ctx context.Context, p *url.URL, target string) (net.Conn, error) {
	addr := p.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(p.Host, "1080")
	}
	conn, err := tlsFPDialerBase.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("连 SOCKS5 代理 %s 失败：%w", addr, err)
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	defer conn.SetDeadline(time.Time{})

	// 1. 问候：无认证 +（可选）用户名密码
	methods := []byte{0x00}
	if p.User != nil {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fail(err)
	}
	var greet [2]byte
	if _, err := io.ReadFull(conn, greet[:]); err != nil {
		return fail(fmt.Errorf("SOCKS5 问候失败：%w", err))
	}
	if greet[0] != 0x05 {
		return fail(fmt.Errorf("SOCKS5 版本不符：0x%02x", greet[0]))
	}
	switch greet[1] {
	case 0x00:
		// 无需认证
	case 0x02:
		if p.User == nil {
			return fail(errors.New("SOCKS5 要求用户名密码，但代理 URL 里没写"))
		}
		user := p.User.Username()
		pass, _ := p.User.Password()
		buf := []byte{0x01, byte(len(user))}
		buf = append(buf, user...)
		buf = append(buf, byte(len(pass)))
		buf = append(buf, pass...)
		if _, err := conn.Write(buf); err != nil {
			return fail(err)
		}
		var rep [2]byte
		if _, err := io.ReadFull(conn, rep[:]); err != nil {
			return fail(fmt.Errorf("SOCKS5 认证失败：%w", err))
		}
		if rep[1] != 0x00 {
			return fail(errors.New("SOCKS5 用户名密码被拒绝"))
		}
	case 0xff:
		return fail(errors.New("SOCKS5 代理不接受任何认证方式"))
	default:
		return fail(fmt.Errorf("SOCKS5 选了不支持的认证方式 0x%02x", greet[1]))
	}

	// 2. CONNECT：域名交给代理解析（与 socks5h 一致）
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fail(fmt.Errorf("目标地址不合法 %q：%w", target, err))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fail(fmt.Errorf("目标端口不合法 %q", portStr))
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fail(errors.New("目标域名过长"))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}

	// 3. 应答：VER REP RSV ATYP + BND.ADDR + BND.PORT
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fail(fmt.Errorf("SOCKS5 CONNECT 应答失败：%w", err))
	}
	if head[0] != 0x05 {
		return fail(fmt.Errorf("SOCKS5 应答版本不符：0x%02x", head[0]))
	}
	if head[1] != 0x00 {
		return fail(fmt.Errorf("SOCKS5 CONNECT 被拒：REP=0x%02x", head[1]))
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fail(err)
		}
		skip = int(l[0])
	default:
		return fail(fmt.Errorf("SOCKS5 应答地址类型未知：0x%02x", head[3]))
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip+2)); err != nil {
		return fail(fmt.Errorf("SOCKS5 应答地址读取失败：%w", err))
	}
	return conn, nil
}
