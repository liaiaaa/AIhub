package main

// MITM CA 与分流代理的单元测试。
//
// 这里刻意不测「真的连 api.anthropic.com」——那要凭据、要额度、还依赖外网。
// 端到端验证放在 mitm_proxy_e2e_test.go（用本地假上游 + 真 TLS 握手）。

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempDataRoot 把 hubRoot 指到临时目录，返回数据根。
// 与 testmain_test.go 的全局隔离叠加，这里是为了「CA 落在哪」可断言。
func withTempDataRoot(t *testing.T) string {
	t.Helper()
	old := hubRoot
	dir := t.TempDir()
	hubRoot = dir
	t.Cleanup(func() { hubRoot = old })
	return dir
}

func TestMITMCAGenerateAndReload(t *testing.T) {
	root := withTempDataRoot(t)

	ca, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("首次生成失败：%v", err)
	}
	if !ca.cert.IsCA {
		t.Fatal("生成的证书不是 CA")
	}
	if ca.cert.Subject.CommonName != mitmCACommonName {
		t.Errorf("CN = %q，期望 %q", ca.cert.Subject.CommonName, mitmCACommonName)
	}
	if ca.cert.MaxPathLen != 0 || !ca.cert.MaxPathLenZero {
		t.Error("CA 应限制 pathlen=0（不能签中间 CA）")
	}

	certPath, keyPath, leafPath := mitmCAPaths()
	for _, p := range []string{certPath, keyPath, leafPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s 未落盘：%v", p, err)
		}
	}
	// 私钥必须 0600，目录必须 0700 —— 这是「私钥不外泄」的最低保证。
	for _, p := range []string{keyPath, leafPath} {
		fi, _ := os.Stat(p)
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s 权限 %o，期望 600", filepath.Base(p), perm)
		}
	}
	di, _ := os.Stat(filepath.Join(root, mitmDirName))
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("mitm 目录权限 %o，期望 700", perm)
	}

	// 再读一次必须是同一张（不能每次启动都换 CA，否则用户装过的信任就失效）。
	ca2, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("二次加载失败：%v", err)
	}
	if ca.fingerprint() != ca2.fingerprint() {
		t.Errorf("两次加载指纹不同：%s vs %s", ca.fingerprint(), ca2.fingerprint())
	}
}

func TestMITMCALeafForHost(t *testing.T) {
	withTempDataRoot(t)
	ca, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("生成 CA 失败：%v", err)
	}

	lc, err := ca.leafFor("api.anthropic.com")
	if err != nil {
		t.Fatalf("签发 leaf 失败：%v", err)
	}
	leaf := lc.Leaf
	if leaf == nil {
		t.Fatal("leaf 证书未解析")
	}
	if err := leaf.VerifyHostname("api.anthropic.com"); err != nil {
		t.Errorf("leaf 不匹配 api.anthropic.com：%v", err)
	}
	if err := leaf.VerifyHostname("evil.example.com"); err == nil {
		t.Error("leaf 不该匹配其他域名")
	}
	// 链里必须带 CA 证书（否则客户端无法验证）。
	if len(lc.Certificate) != 2 {
		t.Errorf("证书链长度 %d，期望 2（leaf + CA）", len(lc.Certificate))
	}
	// 用 CA 验签：这条能证明 leaf 真是这张 CA 签的。
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, DNSName: "api.anthropic.com",
	}); err != nil {
		t.Errorf("leaf 无法用 CA 验证：%v", err)
	}

	// 缓存：同一域名两次拿到同一个指针。
	lc2, _ := ca.leafFor("api.anthropic.com")
	if lc != lc2 {
		t.Error("同一域名未命中 leaf 缓存")
	}
	// 不同域名必须换一张（SAN 不同）。
	lc3, _ := ca.leafFor("example.org")
	if lc3 == lc {
		t.Error("不同域名复用了同一张 leaf")
	}
	if err := lc3.Leaf.VerifyHostname("example.org"); err != nil {
		t.Errorf("example.org 的 leaf 不匹配：%v", err)
	}
}

func TestMITMCALeafNoSNI(t *testing.T) {
	withTempDataRoot(t)
	ca, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("生成 CA 失败：%v", err)
	}
	// 空 SNI 要兜底到 mitmPrimaryHost，不能签出空 CN 的证书。
	lc, err := ca.leafFor("")
	if err != nil {
		t.Fatalf("空 SNI 签发失败：%v", err)
	}
	if err := lc.Leaf.VerifyHostname(mitmPrimaryHost); err != nil {
		t.Errorf("空 SNI 兜底证书不匹配 %s：%v", mitmPrimaryHost, err)
	}
}

func TestMITMCACorruptFilesRegenerate(t *testing.T) {
	withTempDataRoot(t)
	ca1, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("生成 CA 失败：%v", err)
	}
	certPath, _, _ := mitmCAPaths()
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("写坏文件失败：%v", err)
	}
	ca2, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("坏文件时应重新生成，实际报错：%v", err)
	}
	if ca1.fingerprint() == ca2.fingerprint() {
		t.Error("ca.pem 损坏后应生成新 CA")
	}
}

func TestMITMCAPEMIsCertificateOnly(t *testing.T) {
	withTempDataRoot(t)
	ca, err := loadOrCreateMITMCA()
	if err != nil {
		t.Fatalf("生成 CA 失败：%v", err)
	}
	pemText := ca.caPEM()
	block, _ := pem.Decode(pemText)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("caPEM 不是 CERTIFICATE，是 %v", block)
	}
	if strings.Contains(string(pemText), "PRIVATE KEY") {
		t.Fatal("caPEM 里出现了私钥 —— 绝对不能导出给客户端")
	}
	if !strings.Contains(ca.fingerprint(), ":") {
		t.Errorf("指纹格式异常：%s", ca.fingerprint())
	}
}

func TestMITMTargetHostsIsNarrow(t *testing.T) {
	// 只允许解密 Anthropic API 一个域名。这条测试是给未来的人看的：
	// 想往里加域名（比如 claude.ai）之前，先想清楚这是把 chat 也解密了。
	if len(mitmTargetHosts) != 1 {
		t.Errorf("mitmTargetHosts 有 %d 个域名，期望只有 api.anthropic.com", len(mitmTargetHosts))
	}
	if !mitmTargetHosts["api.anthropic.com"] {
		t.Error("缺少 api.anthropic.com")
	}
}

func TestMITMEnvSnippetPointsAtRealCA(t *testing.T) {
	withTempDataRoot(t)
	if _, err := loadOrCreateMITMCA(); err != nil {
		t.Fatalf("生成 CA 失败：%v", err)
	}
	s := mitmEnvSnippet()
	if !strings.Contains(s, "HTTPS_PROXY=http://127.0.0.1:18792") {
		t.Errorf("片段里缺 HTTPS_PROXY：%s", s)
	}
	if !strings.Contains(s, "NODE_EXTRA_CA_CERTS="+mitmCACertPath()) {
		t.Errorf("片段里缺 NODE_EXTRA_CA_CERTS：%s", s)
	}
	if _, err := os.Stat(mitmCACertPath()); err != nil {
		t.Errorf("片段指向的 CA 不存在：%v", err)
	}
}

func TestMITMStatusBeforeStart(t *testing.T) {
	withTempDataRoot(t)
	h := &Hub{}
	st := h.mitmStatusSnapshot()
	if st["running"] != false {
		t.Error("未启动时 running 应为 false")
	}
	if st["ca_exists"] != false {
		t.Error("CA 未生成时 ca_exists 应为 false")
	}
	if st["port"] != mitmPort {
		t.Errorf("port = %v，期望 %d", st["port"], mitmPort)
	}
}

func TestMITMStartStop(t *testing.T) {
	withTempDataRoot(t)
	// 端口若已被占用（开发机上开着），跳过而不是失败。
	if ln, err := net.Listen("tcp", "127.0.0.1:18792"); err == nil {
		ln.Close()
	} else {
		t.Skipf("端口 %d 不可用：%v", mitmPort, err)
	}
	h := &Hub{}
	ok, msg := h.mitmStart()
	if !ok {
		t.Fatalf("启动失败：%s", msg)
	}
	defer h.mitmStop()
	if !h.mitmStatusSnapshot()["running"].(bool) {
		t.Error("启动后 running 应为 true")
	}
	// 重复启动要幂等。
	if ok2, _ := h.mitmStart(); !ok2 {
		t.Error("重复启动应返回 true")
	}
	ok, msg = h.mitmStop()
	if !ok {
		t.Fatalf("停止失败：%s", msg)
	}
	if h.mitmStatusSnapshot()["running"].(bool) {
		t.Error("停止后 running 应为 false")
	}
	// 停止后再停也不报错。
	if ok, _ := h.mitmStop(); !ok {
		t.Error("重复停止应返回 true")
	}
}

// TestMITMUpstreamGoesThroughSystemProxy 上游透传必须走系统代理。
//
// 2026-11-09 实测：本机直连 api.anthropic.com → 403（Cloudflare 拦国内 IP），
// 经系统代理 127.0.0.1:10808 → 401（通了）。原来 Proxy: nil 等于不走代理，
// 透传路径根本连不上上游。
func TestMITMUpstreamGoesThroughSystemProxy(t *testing.T) {
	c := mitmUpstreamClient()
	if c == nil || c.Transport == nil {
		t.Fatal("上游 client 未初始化")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型 %T", c.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("Proxy 是 nil —— 透传会直连，本机会 403")
	}
}

// TestMITMProxySelfLoopGuard 系统代理若指向自己，必须直连（否则自己转自己）。
func TestMITMProxySelfLoopGuard(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{fmt.Sprintf("http://127.0.0.1:%d", mitmPort), true},
		{fmt.Sprintf("http://localhost:%d", mitmPort), true},
		{fmt.Sprintf("http://[::1]:%d", mitmPort), true},
		{"http://127.0.0.1:10808", false},
		{"http://127.0.0.1:7890", false},
		{"socks5://127.0.0.1:10809", false},
	}
	for _, c := range cases {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", c.url, err)
		}
		if got := sameProxyAsSelf(u); got != c.want {
			t.Errorf("sameProxyAsSelf(%s) = %v，期望 %v", c.url, got, c.want)
		}
	}
}
