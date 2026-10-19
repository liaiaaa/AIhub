package main

// MiMo（小米 MiMo 桌面端）认证：小米账号 SSO → serviceToken。
//
// 与 CodeBuddy/WorkBuddy 的「一个 JSON 凭证文件 + Bearer token」不同，MiMo 走的是
// 小米统一登录（passport），需要两步换票：
//
//	Phase 1  GET account.xiaomi.com/pass/serviceLogin?sid=mimopc&_json=true
//	         带 Cookie: passToken/userId/cUserId（桌面端登录后落在 xiaomi-account 分区）
//	         → 返回 {code, location, nonce, ssecurity}
//	Phase 2  clientSign = urlencode(base64(sha1("nonce=<nonce>&<ssecurity>")))
//	         GET <location>&clientSign=...
//	         → Set-Cookie: serviceToken=...
//
// 之后带着 serviceToken + userId 请求 mimo-server-cn.xiaomimimo.com。
//
// 两个容易踩的点：
//  1) nonce 是 19 位整数，超出 float64 精度，必须按字符串原样取（这里用 json.Number）。
//  2) sid 是 "mimopc"——不是 mimo/mimocode，那些会返回 21315 无效的sid。
//     mimopc 是从 mimo-server 302 跳转的 Location 里读出来的。

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	mimoSID      = "mimopc"
	mimoTokenTTL = 30 * time.Minute
	mimoUA       = "MiClaw/1.0"
	mimoSource   = "mimocode-cli-free"
)

// mimoCookiePath 返回 xiaomi-account 分区的 Cookies 库路径。
// MiMo 桌面端把登录 cookie 存在这里；文件被运行中的桌面端占用时读取会失败，
// 调用方需要给出可读的错误提示。
func mimoCookiePath() string {
	return filepath.Join(os.Getenv("APPDATA"), "Xiaomi MiMo",
		"Partitions", "xiaomi-account", "Network", "Cookies")
}

type MimoAuth struct {
	mu       sync.Mutex
	client   *http.Client
	cookies  string // serviceToken + 附加 cookie，直接用于请求
	userId   string
	nickname string
	uid      string
	fetched  time.Time
}

func NewMimoAuth() *MimoAuth {
	return &MimoAuth{client: backendClient(20 * time.Second)}
}

// Invalidate 丢弃缓存，下次请求重新换票。
func (m *MimoAuth) Invalidate() {
	m.mu.Lock()
	m.fetched = time.Time{}
	m.mu.Unlock()
}

func (m *MimoAuth) expiredLocked() bool {
	return m.cookies == "" || time.Since(m.fetched) > mimoTokenTTL
}

// Headers 返回调用 mimo-server 需要的请求头（Cookie + X-Mimo-Source）。
func (m *MimoAuth) Headers() (map[string]string, error) {
	if err := m.ensure(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	ck := m.cookies
	m.mu.Unlock()
	return map[string]string{
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"Cookie":        ck,
		"X-Mimo-Source": mimoSource,
		"User-Agent":    mimoUA,
	}, nil
}

func (m *MimoAuth) Summary() (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cookies == "" {
		return map[string]any{"uid": "", "nickname": "", "enterpriseName": "", "token_expired": true}, nil
	}
	return map[string]any{
		"uid":              m.userId,
		"nickname":         m.nickname,
		"enterpriseName":   "",
		"token_expires_at": m.fetched.Add(mimoTokenTTL).UnixMilli(),
		"token_expired":    m.expiredLocked(),
	}, nil
}

// ensure 保证有一份可用的 serviceToken。
func (m *MimoAuth) ensure() error {
	m.mu.Lock()
	if !m.expiredLocked() {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// 换票是网络操作，放在锁外，避免卡住控制台（与 auth.go 的教训一致）。
	cookies, userId, err := m.exchange()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cookies, m.userId = cookies, userId
	m.fetched = time.Now()
	m.mu.Unlock()
	return nil
}

// exchange 执行完整的 Phase 1 + Phase 2 换票流程。
func (m *MimoAuth) exchange() (string, string, error) {
	path := mimoCookiePath()
	ck, err := readCookies(path, "passToken", "userId", "cUserId")
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("未找到 MiMo 登录数据，请先启动并登录 MiMo 桌面端")
		}
		return "", "", fmt.Errorf("读取 MiMo 登录数据失败（若 MiMo 正在运行请先退出再试）：%v", err)
	}
	passToken, userId, cUserId := ck["passToken"], ck["userId"], ck["cUserId"]
	if passToken == "" || userId == "" {
		return "", "", fmt.Errorf("MiMo 登录数据不完整，请在桌面端重新登录")
	}

	// ---- Phase 1 ----
	q := url.Values{}
	q.Set("_locale", "zh_CN")
	q.Set("_snsNone", "true")
	q.Set("sid", mimoSID)
	q.Set("_json", "true")
	phase1 := "https://account.xiaomi.com/pass/serviceLogin?" + q.Encode()

	req, err := http.NewRequest("GET", phase1, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Cookie", fmt.Sprintf("passToken=%s; userId=%s; cUserId=%s", passToken, userId, cUserId))
	req.Header.Set("User-Agent", mimoUA)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("MiMo 登录 Phase1 网络失败：%v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("MiMo 登录 Phase1 HTTP %d", resp.StatusCode)
	}
	text := strings.TrimPrefix(string(body), "&&&START&&&")

	// 用 json.Number 保住 nonce 的精度。
	var p1 struct {
		Code      int         `json:"code"`
		Desc      string      `json:"desc"`
		Location  string      `json:"location"`
		Nonce     json.Number `json:"nonce"`
		Ssecurity string      `json:"ssecurity"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&p1); err != nil {
		return "", "", fmt.Errorf("MiMo 登录 Phase1 响应解析失败：%v", err)
	}
	if p1.Code != 0 {
		return "", "", fmt.Errorf("MiMo 登录失败（code=%d %s），请重新登录桌面端", p1.Code, p1.Desc)
	}
	if p1.Location == "" {
		return "", "", fmt.Errorf("MiMo 登录未返回跳转地址")
	}

	// ---- Phase 2 ----
	signInput := "nonce=" + p1.Nonce.String()
	if s := strings.TrimSpace(p1.Ssecurity); s != "" {
		signInput += "&" + s
	}
	sum := sha1.Sum([]byte(signInput))
	clientSign := url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))

	req2, err := http.NewRequest("GET", p1.Location+"&clientSign="+clientSign, nil)
	if err != nil {
		return "", "", err
	}
	req2.Header.Set("User-Agent", mimoUA) // Phase2 故意不带 Cookie
	resp2, err := m.client.Do(req2)
	if err != nil {
		return "", "", fmt.Errorf("MiMo 登录 Phase2 网络失败：%v", err)
	}
	io.Copy(io.Discard, io.LimitReader(resp2.Body, 1<<20))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("MiMo 登录 Phase2 HTTP %d", resp2.StatusCode)
	}

	serviceToken := ""
	extra := []string{}
	for _, sc := range resp2.Cookies() {
		switch sc.Name {
		case "serviceToken", mimoSID + "_serviceToken":
			serviceToken = sc.Value
		case "userId":
			if sc.Value != "" {
				userId = sc.Value
			}
		default:
			if sc.Value != "" {
				extra = append(extra, sc.Name+"="+sc.Value)
			}
		}
	}
	if serviceToken == "" {
		return "", "", fmt.Errorf("MiMo 登录未取到 serviceToken，请在桌面端重新登录")
	}
	parts := append([]string{"serviceToken=" + serviceToken, "userId=" + userId}, extra...)
	return strings.Join(parts, "; "), userId, nil
}

// mimoProbe 供控制台/健康检查使用：验证当前凭据能否换票成功。
func (m *MimoAuth) Probe() error {
	m.Invalidate()
	return m.ensure()
}

// ---- 便于调试：手工跑一次换票 ----

func mimoDebugExchange() string {
	var buf bytes.Buffer
	a := NewMimoAuth()
	ck, uid, err := a.exchange()
	if err != nil {
		return "ERR: " + err.Error()
	}
	fmt.Fprintf(&buf, "userId=%s\ncookie len=%d\ncookie=%s\n", uid, len(ck), ck)
	return buf.String()
}
