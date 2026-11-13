package main

// 果办（OfficeAce）浏览器 OAuth 登录：把 dsh 插件的登录能力搬进 aihub，
// 让 aihub 不再依赖插件（插件可以卸载）。
//
// 协议（全部实测自 dsh-connect-huaweicloud@0.0.5 源码，2026-11-13）：
//  1. PKCE：verifier = 64 字节随机数 base64url；challenge = SHA-256(verifier) base64url。
//     **method 必须写成 "SHA-256"**（写成 "S256" 会被 IAM 报 IAM.0007，实测坑）。
//  2. 授权地址套了一层登录页：
//     https://auth.huaweicloud.com/authui/login.html?service=<整条 encode 的 inner>
//     inner = https://auth.huaweicloud.com/authui/v1/oauth2/authorize?client_id=…&…
//     内层参数只 encode 一次，service 再整体 encode 一次（双重编码，别漏）。
//  3. 拿 code 有两条路：
//     - 路线 A（本机回环）：监听 127.0.0.1 随机端口，回调路径 /oauth/callback?code=…
//     - 路线 B（云端）：先 POST {CLAW}/v1/claw/auth/state 登记 state，
//       再轮询 {CLAW}/v1/claw/auth/code?state=… 取 code（不需要本机端口）
//     优先 A（快、无轮询），A 起不来再 B。
//  4. 换 token：POST sts.cn-north-4…/v1/oauth2/tokens
//     grant_type=authorization_code + code + code_verifier + client_id + redirect_uri，
//     头带 DPoP（**私钥在登录时新生成，与 refresh_token 绑定**——换一把会报
//     InvalidDPoPHeader，所以私钥必须随凭据一起落盘）。
//  5. 落库：账号身份从 refresh_token 的 JWT payload.user_profile 解出
//     （account_id=domainId / principal_id=userId / account_name=userName）。
//
// 与插件的差异：aihub 走「路线 A 优先」，因为控制台是本机页面；用户点登录后
// 浏览器打开授权页，回调打回本机回环端口，控制台轮询 /api/huawei/login/status。

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	hwAuthorizeInner = "https://auth.huaweicloud.com/authui/v1/oauth2/authorize"
	hwAuthorizePage  = "https://auth.huaweicloud.com/authui/login.html?service="
	hwLoopbackPath   = "/oauth/callback"
	hwLoginTimeout   = 5 * time.Minute
)

// hwLoginSession 一次进行中的登录（全局单例：同一时刻只允许一个登录流程）。
type hwLoginSession struct {
	mu        sync.Mutex
	phase     string // idle | preparing | awaiting-user | exchanging | done | failed
	detail    string
	authURL   string
	redirect  string
	startedAt time.Time
	cancel    chan struct{}
	codeCh    chan string
	ln        net.Listener
	account   *hwAccount // done 后指向新落库的账号
}

var hwLogin = &hwLoginSession{phase: "idle"}

// hwLoginState 给控制台看的登录状态快照。
func hwLoginState() map[string]any {
	hwLogin.mu.Lock()
	defer hwLogin.mu.Unlock()
	out := map[string]any{
		"phase":  hwLogin.phase,
		"detail": hwLogin.detail,
	}
	if hwLogin.authURL != "" {
		out["authorize_url"] = hwLogin.authURL
	}
	if !hwLogin.startedAt.IsZero() {
		out["started_at"] = hwLogin.startedAt.Format("15:04:05")
	}
	return out
}

// hwLoginStart 发起一次登录。返回授权地址，前端拿去开浏览器。
//
// 立刻返回（不在内部等用户授权）：用户可能要在浏览器里输密码/扫码，耗时不定。
// 真正的完成靠后台 goroutine，前端轮询 hwLoginState。
func hwLoginStart() (string, error) {
	hwLogin.mu.Lock()
	if hwLogin.phase == "preparing" || hwLogin.phase == "awaiting-user" || hwLogin.phase == "exchanging" {
		url := hwLogin.authURL
		hwLogin.mu.Unlock()
		return url, fmt.Errorf("已有登录流程在进行中")
	}
	// 重置会话
	hwLogin.phase = "preparing"
	hwLogin.detail = "正在准备授权…"
	hwLogin.authURL = ""
	hwLogin.redirect = ""
	hwLogin.account = nil
	hwLogin.startedAt = time.Now()
	hwLogin.cancel = make(chan struct{})
	hwLogin.codeCh = make(chan string, 1)
	cancel := hwLogin.cancel
	codeCh := hwLogin.codeCh
	hwLogin.mu.Unlock()

	// ① PKCE
	verifierRaw := make([]byte, 64)
	if _, err := rand.Read(verifierRaw); err != nil {
		hwLoginFail("随机数生成失败：" + err.Error())
		return "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	stateRaw := make([]byte, 16)
	rand.Read(stateRaw)
	state := fmt.Sprintf("%x", stateRaw)

	// ② 起本机回环（路线 A）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		hwLoginFail("本机回环端口分配失败：" + err.Error())
		return "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d%s", port, hwLoopbackPath)
	hwLogin.mu.Lock()
	hwLogin.ln = ln
	hwLogin.redirect = redirect
	hwLogin.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc(hwLoopbackPath, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "<h3>授权未返回 code，请重试。</h3>")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<h3>授权成功，可以关闭此页面回到 aihub。</h3>")
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		defer recoverGoroutine("huawei_login.go:loopback")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			// 登录结束关监听时这里是正常路径，不刷日志
		}
	}()

	// ③ 拼授权地址（双层编码）
	inner := hwAuthorizeInner + "?" + url.Values{
		"client_id":             {hwClientID},
		"code_challenge":        {challenge},
		"code_challenge_method": {"SHA-256"}, // 必须是 SHA-256，不是 S256
		"state":                 {state},
		"scope":                 {"openid"},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
	}.Encode()
	authURL := hwAuthorizePage + url.QueryEscape(inner)

	hwLogin.mu.Lock()
	hwLogin.authURL = authURL
	hwLogin.phase = "awaiting-user"
	hwLogin.detail = "等待浏览器授权（若浏览器没自动打开，手动访问下方链接）"
	hwLogin.mu.Unlock()

	// ④ 后台等 code → 换 token → 落库
	go func() {
		defer recoverGoroutine("huawei_login.go:flow")
		var code string
		select {
		case code = <-codeCh:
		case <-cancel:
			hwLoginFail("登录已取消")
			return
		case <-time.After(hwLoginTimeout):
			hwLoginFail("登录超时（5 分钟未完成授权）")
			return
		}
		hwLogin.mu.Lock()
		hwLogin.phase = "exchanging"
		hwLogin.detail = "正在换取凭据…"
		hwLogin.mu.Unlock()

		acct, err := hwExchangeCode(code, verifier, redirect)
		if err != nil {
			hwLoginFail("换取凭据失败：" + err.Error())
			return
		}
		hwLogin.mu.Lock()
		hwLogin.phase = "done"
		hwLogin.detail = "登录成功：" + acct.userName
		hwLogin.account = &acct
		hwLogin.mu.Unlock()
		logHub("果办登录成功：%s（%s）", acct.userName, acct.label)
	}()

	logHub("果办登录已发起，等待浏览器授权（回环 %s）", redirect)
	return authURL, nil
}

// hwLoginCancel 取消进行中的登录。
func hwLoginCancel() {
	hwLogin.mu.Lock()
	c := hwLogin.cancel
	phase := hwLogin.phase
	hwLogin.mu.Unlock()
	if c != nil && (phase == "awaiting-user" || phase == "preparing") {
		select {
		case <-c:
		default:
			close(c)
		}
	}
	hwCleanupLogin()
}

// hwCleanupLogin 关掉回环监听（幂等）。
func hwCleanupLogin() {
	hwLogin.mu.Lock()
	ln := hwLogin.ln
	hwLogin.ln = nil
	hwLogin.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
}

// hwLoginFail 标记失败并清理。
func hwLoginFail(msg string) {
	hwLogin.mu.Lock()
	hwLogin.phase = "failed"
	hwLogin.detail = msg
	hwLogin.mu.Unlock()
	hwCleanupLogin()
	logHub("果办登录失败：%s", msg)
}

// hwExchangeCode 用授权码换 STS 凭据并落库。
func hwExchangeCode(code, verifier, redirect string) (hwAccount, error) {
	// ① 新生成 DPoP 私钥（与 refresh_token 绑定，必须随凭据落盘）
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return hwAccount{}, fmt.Errorf("DPoP 私钥生成失败: %w", err)
	}
	dpop, err := hwDpopJWT(priv, hwSTSRefreshURL, "POST")
	if err != nil {
		return hwAccount{}, err
	}

	// ② 换 token
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {hwClientID},
		"redirect_uri":  {redirect},
	}
	req, err := http.NewRequest("POST", hwSTSRefreshURL, strings.NewReader(form.Encode()))
	if err != nil {
		return hwAccount{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", dpop)
	resp, err := backendClient(30 * time.Second).Do(req)
	if err != nil {
		return hwAccount{}, fmt.Errorf("网络错误: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return hwAccount{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var j struct {
		Credentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		return hwAccount{}, fmt.Errorf("响应解析失败: %w", err)
	}
	if j.Credentials.AccessKeyID == "" || j.Credentials.SecurityToken == "" {
		return hwAccount{}, fmt.Errorf("响应缺少凭据")
	}
	if j.RefreshToken == "" {
		return hwAccount{}, fmt.Errorf("响应缺少 refresh_token（无法续期）")
	}

	// ③ 从 refresh_token 的 JWT 解账号身份
	prof := hwParseUserProfile(j.RefreshToken)
	if prof.DomainID == "" && prof.UserID == "" {
		return hwAccount{}, fmt.Errorf("无法从 refresh_token 解析账号身份")
	}

	// ④ 落库（与插件同格式，插件读得懂 → 可共存）
	expires := time.Now().Add(90 * time.Minute)
	if t, perr := time.Parse(time.RFC3339Nano, j.Credentials.Expiration); perr == nil {
		expires = t
	}
	accountKey := prof.UserID
	if accountKey == "" {
		accountKey = prof.UserName
	}
	id := hwAccountIDFor(accountKey)
	acct := hwAccount{
		id:      id,
		label:   prof.DomainID,
		refresh: j.RefreshToken,
		sts: hwSTSFields{
			AccessKeyID:     j.Credentials.AccessKeyID,
			SecretAccessKey: j.Credentials.SecretAccessKey,
			SecurityToken:   j.Credentials.SecurityToken,
			ExpiresAt:       expires,
			RefreshToken:    j.RefreshToken,
		},
	}
	if err := hwSaveNewAccount(acct, prof, priv); err != nil {
		return hwAccount{}, fmt.Errorf("写 vault 失败: %w", err)
	}
	// 顺手设成选中账号
	hwSelectAccount(id)
	return acct, nil
}

// hwUserProfile refresh_token JWT 里的账号身份。
type hwUserProfile struct {
	DomainID    string // account_id
	UserID      string // principal_id
	UserName    string // account_name
	PrincipalUR string
}

// hwParseUserProfile 解 refresh_token 的 JWT payload.user_profile。
//
// refresh_token 形如 xxx.yyy.zzz（JWT，签名段不校验——只读取我们自己拿到的凭据
// 里的声明，不用于信任判定，真伪由华为 STS 在续期时校验）。
func hwParseUserProfile(refreshToken string) hwUserProfile {
	parts := strings.Split(refreshToken, ".")
	if len(parts) < 2 {
		return hwUserProfile{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return hwUserProfile{}
		}
	}
	var doc struct {
		UserProfile json.RawMessage `json:"user_profile"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil || len(doc.UserProfile) == 0 {
		return hwUserProfile{}
	}
	// user_profile 可能是 JSON 对象，也可能是 base64 编码的 JSON 字符串（两种都实测过）
	raw := doc.UserProfile
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if dec, derr := base64.StdEncoding.DecodeString(s); derr == nil {
				raw = dec
			} else if dec, derr := base64.RawStdEncoding.DecodeString(s); derr == nil {
				raw = dec
			}
		}
	}
	var p struct {
		AccountID    string `json:"account_id"`
		AccountName  string `json:"account_name"`
		PrincipalID  string `json:"principal_id"`
		PrincipalURN string `json:"principal_urn"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return hwUserProfile{}
	}
	return hwUserProfile{
		DomainID:    p.AccountID,
		UserID:      p.PrincipalID,
		UserName:    p.AccountName,
		PrincipalUR: p.PrincipalURN,
	}
}

// hwAccountIDFor 由账号身份派生稳定 id（与插件算法一致：acc_ + sha256 前 16 位）。
func hwAccountIDFor(accountKey string) string {
	sum := sha256.Sum256([]byte(accountKey))
	return "acc_" + fmt.Sprintf("%x", sum[:8])
}

// hwSaveNewAccount 把新登录的账号写进 vault（格式与 dsh 插件完全一致）。
func hwSaveNewAccount(a hwAccount, prof hwUserProfile, priv *ecdsa.PrivateKey) error {
	dir := hwVaultPath()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	xB := priv.PublicKey.X.FillBytes(make([]byte, 32))
	yB := priv.PublicKey.Y.FillBytes(make([]byte, 32))
	dB := priv.D.FillBytes(make([]byte, 32))
	jwk := map[string]string{
		"kty": "EC", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(xB),
		"y": base64.RawURLEncoding.EncodeToString(yB),
		"d": base64.RawURLEncoding.EncodeToString(dB),
	}
	now := time.Now().UnixMilli()
	doc := map[string]any{
		"schemaVersion": 1,
		"account": map[string]any{
			"id":         a.id,
			"accountKey": strings.TrimPrefix(a.id, "acc_"),
			"label":      a.label,
			"userId":     prof.UserID,
			"userName":   prof.UserName,
			"origin":     "login",
			"sts": map[string]any{
				"accessKeyId":     a.sts.AccessKeyID,
				"secretAccessKey": a.sts.SecretAccessKey,
				"securityToken":   a.sts.SecurityToken,
				"projectId":       "",
				"expiresAt":       a.sts.ExpiresAt.UTC().Format(time.RFC3339Nano),
				"refreshToken":    a.refresh,
				"dpopPrivateJwk":  jwk,
				"domainId":        prof.DomainID,
				"userId":          prof.UserID,
				"userName":        prof.UserName,
			},
			"savedAtMs":  now,
			"syncedAtMs": now,
		},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	file := filepath.Join(dir, a.id+".json")
	tmp := file + ".tmp-login"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// hwSelectAccount 写入 _selected.json（与插件同一文件，双方读得到）。
func hwSelectAccount(id string) {
	if !hwValidAccountID(id) {
		return
	}
	dir := hwVaultPath()
	out, _ := json.Marshal(map[string]string{"selectedId": id})
	tmp := filepath.Join(dir, ".tmp-selected")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	os.Rename(tmp, filepath.Join(dir, "_selected.json"))
}

// hwSelectedAccountID 读 _selected.json；空 = 没选过（用第一个账号）。
func hwSelectedAccountID() string {
	raw, err := os.ReadFile(filepath.Join(hwVaultPath(), "_selected.json"))
	if err != nil {
		return ""
	}
	var v struct {
		SelectedID string `json:"selectedId"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.SelectedID
}

// hwValidAccountID 只接受 acc_[0-9a-f]{16}，避免路径穿越。
func hwValidAccountID(id string) bool {
	if len(id) != 20 || !strings.HasPrefix(id, "acc_") {
		return false
	}
	for _, c := range id[4:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
