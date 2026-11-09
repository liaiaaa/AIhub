package main

// 本地 MITM CA：给「Claude Desktop 的 Code 子进程」签发 api.anthropic.com 的
// 临时证书，用来在本地解密 Code 请求、按 model 分流（见 mitm_proxy.go）。
//
// 为什么需要它：Code 子进程只认 HTTPS_PROXY + NODE_EXTRA_CA_CERTS 这条路
// （Anthropic 文档里的企业代理写法）。要让代理看到请求体里的 model 字段，
// 必须先终止 TLS —— 那就得有一张客户端信任的 CA。
//
// 边界（刻意收窄）：
//   - CA 私钥只落在数据根 mitm/ca.key，权限 0600，不写进仓库、不进日志；
//   - leaf 只签「客户端实际请求的那个域名」，不做通配；
//   - 只在 mitm_proxy.go 明确列出的目标域名上启用解密，其余 CONNECT 纯隧道。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	mitmDirName       = "mitm"
	mitmCACertName    = "ca.pem"
	mitmCAKeyName     = "ca.key"
	mitmCALeafKeyName = "leaf.key"
	mitmCACommonName  = "AIHub Local Proxy CA"
	mitmCAOrgName     = "AIHub Local (not a public CA)"
	mitmCAValidYears  = 10
	mitmLeafValidDays = 30
)

// mitmCA 是「根 CA + 按域名现签 leaf」的组合。leaf 私钥全域名共用一把，
// 只换证书里的 SAN —— 每域名一把密钥对握手要多一次 ECDSA 生成，没必要。
type mitmCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	leafKey *ecdsa.PrivateKey
	pemText []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// mitmCAPaths 返回 (certPath, keyPath, leafKeyPath)。
func mitmCAPaths() (string, string, string) {
	dir := dataPath(mitmDirName)
	return filepath.Join(dir, mitmCACertName),
		filepath.Join(dir, mitmCAKeyName),
		filepath.Join(dir, mitmCALeafKeyName)
}

// mitmCACertPath 是写进 NODE_EXTRA_CA_CERTS 的那个路径。
func mitmCACertPath() string {
	p, _, _ := mitmCAPaths()
	return p
}

// loadOrCreateMITMCA 读现有 CA；不存在（或文件损坏）就生成一套新的。
// 返回的 mitmCA 自带 PEM 文本，可直接给用户装信任。
func loadOrCreateMITMCA() (*mitmCA, error) {
	certPath, keyPath, leafKeyPath := mitmCAPaths()

	certPEM, cerr := os.ReadFile(certPath)
	keyPEM, kerr := os.ReadFile(keyPath)
	leafPEM, lerr := os.ReadFile(leafKeyPath)

	if cerr == nil && kerr == nil && lerr == nil {
		ca, err := parseMITMCA(certPEM, keyPEM, leafPEM)
		if err == nil {
			return ca, nil
		}
		// 文件在但解析不了：重新生成，别让用户卡在一个坏 CA 上。
		logHub("MITM CA 解析失败，重新生成：%v", err)
	}

	return generateMITMCA(certPath, keyPath, leafKeyPath)
}

func parseMITMCA(certPEM, keyPEM, leafPEM []byte) (*mitmCA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("ca.pem 不是 PEM 证书")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca.pem 解析失败：%w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("ca.key 不是 PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca.key 解析失败：%w", err)
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ca.key 不是 ECDSA 私钥")
	}
	lb, _ := pem.Decode(leafPEM)
	if lb == nil {
		return nil, fmt.Errorf("leaf.key 不是 PEM")
	}
	leafAny, err := x509.ParsePKCS8PrivateKey(lb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("leaf.key 解析失败：%w", err)
	}
	leafKey, ok := leafAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("leaf.key 不是 ECDSA 私钥")
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("ca.pem 不是 CA 证书")
	}
	return &mitmCA{cert: cert, key: key, leafKey: leafKey,
		pemText: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

func generateMITMCA(certPath, keyPath, leafKeyPath string) (*mitmCA, error) {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return nil, fmt.Errorf("创建 MITM 目录失败：%w", err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 CA 私钥失败：%w", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 leaf 私钥失败：%w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: mitmCACommonName, Organization: []string{mitmCAOrgName}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(mitmCAValidYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("签发 CA 证书失败：%w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("回读 CA 证书失败：%w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caKeyPEM, err := marshalKeyPEM(caKey)
	if err != nil {
		return nil, err
	}
	leafKeyPEM, err := marshalKeyPEM(leafKey)
	if err != nil {
		return nil, err
	}
	// 0600：私钥只给本用户读。目录 0700。
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("写 ca.pem 失败：%w", err)
	}
	if err := os.WriteFile(keyPath, caKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("写 ca.key 失败：%w", err)
	}
	if err := os.WriteFile(leafKeyPath, leafKeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("写 leaf.key 失败：%w", err)
	}
	logHub("MITM CA 已生成：%s", certPath)
	return &mitmCA{cert: cert, key: caKey, leafKey: leafKey,
		pemText: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

func marshalKeyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("序列化私钥失败：%w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成序列号失败：%w", err)
	}
	return n, nil
}

// leafFor 给 host 现签（或取缓存）一张 leaf 证书。host 为空时用
// mitmPrimaryHost —— TLS 客户端没发 SNI 的情况。
func (c *mitmCA) leafFor(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		host = mitmPrimaryHost
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaves == nil {
		c.leaves = map[string]*tls.Certificate{}
	}
	if lc, ok := c.leaves[host]; ok {
		return lc, nil
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, mitmLeafValidDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("签发 %s 的 leaf 失败：%w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("回读 %s 的 leaf 失败：%w", host, err)
	}
	tc := &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  c.leafKey,
		Leaf:        leaf,
	}
	c.leaves[host] = tc
	return tc, nil
}

// caPEM 返回 CA 证书的 PEM 文本（给「导出证书」按钮 / 自检用）。
func (c *mitmCA) caPEM() []byte {
	out := make([]byte, len(c.pemText))
	copy(out, c.pemText)
	return out
}

// fingerprint 返回 CA 的 SHA-256 指纹（冒号分隔大写十六进制），
// 方便用户核对装进去的是不是同一张。
func (c *mitmCA) fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
