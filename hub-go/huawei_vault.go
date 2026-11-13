package main

// 果办 vault 读取 + STS 续期。
//
// vault 与 dsh 插件（dsh-connect-huaweicloud）共享：
//   ~/.dsh/.huaweicloud-connect/officeace-vault/acc_*.json
// 结构（2932B 实测）：
//   { account: { id, accountKey, label, userId, userName, origin: "login",
//     sts: { accessKeyId, secretAccessKey, securityToken, projectId,
//            expiresAt, refreshToken, dpopPrivateJwk {kty,crv,x,y,d} },
//     domainId, userId, userName }, savedAtMs, syncedAtMs }
//
// STS 1.5 小时过期；续期 = POST sts.cn-north-4…/v1/oauth2/tokens
//   grant_type=refresh_token + client_id=pdp5_for_agentarts + DPoP(ES256, vault 私钥)。
// refresh_token 是**一次性**的：新的下发旧的失效。aihub 与 dsh 插件共享同一份
// vault，续期竞态的处理是「续期前重读、续期后立刻原子写回；invalid_grant 时
// 再重读一次（可能对方刚续完）」。窗口秒级，可接受；真顶掉了就去 dsh 重登一次。
//
// 私钥是 JWK 形式的 EC P-256。DPoP 签名用 ES256 + raw r||s（低 S 归一化，
// magpie/officeace-auth 同款）；Go 标准库 ecdsa.SignASN1 是 ASN.1 DER 格式，
// 要手动转 raw 再自己处理 S。

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	hwVaultDir        = ".huaweicloud-connect/officeace-vault"
	hwClientID        = "pdp5_for_agentarts"
	hwSTSRefreshURL   = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	hwRefreshSlackSec = 600 // 剩余 <10 分钟就续
)

var hwVaultMu sync.Mutex // 续期是「读-换-写」序列，进程内先自己排开

// hwVaultPath vault 目录（DSH_HOME 可覆盖 $HOME/.dsh）。
func hwVaultPath() string {
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = filepath.Join(os.Getenv("HOME"), ".dsh")
	}
	return filepath.Join(root, hwVaultDir)
}

// hwAccount 一份果办账号（vault 里一个 JSON）。
type hwAccount struct {
	file     string
	id       string
	label    string // domainId（account_id）
	userID   string // principal_id
	userName string // account_name
	sts      hwSTSFields
	refresh  string
	priv     *ecdsa.PrivateKey
}

type hwSTSFields struct {
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	ProjectID       string
	ExpiresAt       time.Time
	RefreshToken    string
}

// hwLoadAccounts 读 vault 里全部账号（当前实测只有一个，但代码按多账号写，
// 与 Qoder 多账号同一姿势——将来加号不用改结构）。
// 返回空列表 = 未登录（vault 不存在或没文件）。
func hwLoadAccounts() []hwAccount {
	dir := hwVaultPath()
	matches, _ := filepath.Glob(filepath.Join(dir, "acc_*.json"))
	accounts := make([]hwAccount, 0, len(matches))
	for _, f := range matches {
		a, err := hwLoadAccount(f)
		if err != nil {
			logHub("果办：读取账号文件 %s 失败：%v", filepath.Base(f), err)
			continue
		}
		accounts = append(accounts, a)
	}
	return accounts
}

func hwLoadAccount(file string) (hwAccount, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return hwAccount{}, err
	}
	var v struct {
		Account struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			DomainID string `json:"domainId"`
			Sts      struct {
				AccessKeyID     string `json:"accessKeyId"`
				SecretAccessKey string `json:"secretAccessKey"`
				SecurityToken   string `json:"securityToken"`
				ProjectID       string `json:"projectId"`
				ExpiresAt       string `json:"expiresAt"`
				RefreshToken    string `json:"refreshToken"`
				UserID          string `json:"userId"`
				UserName        string `json:"userName"`
				DomainID        string `json:"domainId"`
				DpopPrivateJwk  struct {
					Kty string `json:"kty"`
					Crv string `json:"crv"`
					X   string `json:"x"`
					Y   string `json:"y"`
					D   string `json:"d"`
				} `json:"dpopPrivateJwk"`
			} `json:"sts"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return hwAccount{}, fmt.Errorf("vault JSON 解析失败: %w", err)
	}
	a := hwAccount{
		file:     file,
		id:       v.Account.ID,
		label:    v.Account.Label,
		refresh:  v.Account.Sts.RefreshToken,
		userID:   v.Account.Sts.UserID,
		userName: v.Account.Sts.UserName,
		sts: hwSTSFields{
			AccessKeyID:     v.Account.Sts.AccessKeyID,
			SecretAccessKey: v.Account.Sts.SecretAccessKey,
			SecurityToken:   v.Account.Sts.SecurityToken,
			ProjectID:       v.Account.Sts.ProjectID,
			RefreshToken:    v.Account.Sts.RefreshToken,
		},
	}
	if t, err := time.Parse(time.RFC3339Nano, v.Account.Sts.ExpiresAt); err == nil {
		a.sts.ExpiresAt = t
	}
	// DPoP 私钥（EC P-256, JWK）
	if v.Account.Sts.DpopPrivateJwk.D != "" && v.Account.Sts.DpopPrivateJwk.Crv == "P-256" {
		xB, err1 := base64.RawURLEncoding.DecodeString(v.Account.Sts.DpopPrivateJwk.X)
		yB, err2 := base64.RawURLEncoding.DecodeString(v.Account.Sts.DpopPrivateJwk.Y)
		dB, err3 := base64.RawURLEncoding.DecodeString(v.Account.Sts.DpopPrivateJwk.D)
		if err1 == nil && err2 == nil && err3 == nil && len(xB) == 32 && len(yB) == 32 {
			priv := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xB), Y: new(big.Int).SetBytes(yB)}}
			// D 可能带前导零补齐 32 字节（JWK 规范），big.Int 自会处理
			priv.D = new(big.Int).SetBytes(dB)
			a.priv = priv
		}
	}
	if a.label == "" {
		a.label = v.Account.DomainID
	}
	return a, nil
}

// hwFreshSTS 拿一份「可用」的 STS：过期（含 10 分钟余量）就续期并写回 vault。
// 多账号时只处理第一个（当前实测单账号）；竞态处理见文件头注释。
// 返回的凭据可直用于签名层；错误文案给控制台看。
func hwFreshSTS() (hwSTS, string, error) {
	hwVaultMu.Lock()
	defer hwVaultMu.Unlock()
	return hwFreshSTSLocked()
}

func hwFreshSTSLocked() (hwSTS, string, error) {
	accounts := hwLoadAccounts()
	if len(accounts) == 0 {
		return hwSTS{}, "", fmt.Errorf("未找到果办账号（vault=%s）。在控制台点「果办登录」", hwVaultPath())
	}
	// 优先用 _selected.json 选中的账号（与 dsh 插件共享同一个选择）；
	// 没选过或选中的文件没了 → 退回第一个。
	a := accounts[0]
	if sel := hwSelectedAccountID(); sel != "" {
		for _, cand := range accounts {
			if cand.id == sel {
				a = cand
				break
			}
		}
	}
	if time.Until(a.sts.ExpiresAt) > hwRefreshSlackSec*time.Second {
		return hwSTS{AccessKeyID: a.sts.AccessKeyID, SecretAccessKey: a.sts.SecretAccessKey, SecurityToken: a.sts.SecurityToken}, a.label, nil
	}
	// ── 续期 ──
	if a.priv == nil || a.refresh == "" {
		return hwSTS{}, a.label, fmt.Errorf("果办凭据已过期且 vault 里没有可用的续期材料（refresh_token/DPoP 私钥缺失），请在 DSH 里重新登录")
	}
	cred, newRefresh, err := hwRefreshToken(a)
	if err != nil {
		// 竞态兜底：重读一次 vault——可能 dsh 插件刚续完（我们的 refresh_token 已被顶掉）
		if a2, err2 := hwLoadAccount(a.file); err2 == nil && time.Until(a2.sts.ExpiresAt) > hwRefreshSlackSec*time.Second {
			return hwSTS{AccessKeyID: a2.sts.AccessKeyID, SecretAccessKey: a2.sts.SecretAccessKey, SecurityToken: a2.sts.SecurityToken}, a2.label, nil
		}
		return hwSTS{}, a.label, fmt.Errorf("果办续期失败: %w", err)
	}
	// 写回（原子：临时文件 + rename），保留其余字段
	if err := hwWriteBackRefresh(a.file, cred, newRefresh); err != nil {
		logHub("果办：续期成功但写回 vault 失败（下次将重走续期）：%v", err)
	}
	return cred, a.label, nil
}

// hwRefreshToken 用 refresh_token + DPoP 换新 STS。
func hwRefreshToken(a hwAccount) (hwSTS, string, error) {
	dpop, err := hwDpopJWT(a.priv, hwSTSRefreshURL, "POST")
	if err != nil {
		return hwSTS{}, "", err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.refresh},
		"client_id":     {hwClientID},
	}
	req, err := http.NewRequest("POST", hwSTSRefreshURL, strings.NewReader(form.Encode()))
	if err != nil {
		return hwSTS{}, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", dpop)
	cli := backendClient(30 * time.Second)
	resp, err := cli.Do(req)
	if err != nil {
		return hwSTS{}, "", fmt.Errorf("网络错误: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return hwSTS{}, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
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
		return hwSTS{}, "", fmt.Errorf("响应解析失败: %w", err)
	}
	if j.Credentials.AccessKeyID == "" || j.Credentials.SecurityToken == "" {
		return hwSTS{}, "", fmt.Errorf("续期响应缺少凭据")
	}
	var expires time.Time
	if t, perr := time.Parse(time.RFC3339Nano, j.Credentials.Expiration); perr == nil {
		expires = t
	}
	if expires.IsZero() {
		// 上游没给 expiration 时按华为 STS 标准时长兜底（1.5h，留 5 分钟安全余量）
		expires = time.Now().Add(85 * time.Minute)
	}
	return hwSTS{
		AccessKeyID:     j.Credentials.AccessKeyID,
		SecretAccessKey: j.Credentials.SecretAccessKey,
		SecurityToken:   j.Credentials.SecurityToken,
		ExpiresAt:       expires,
	}, j.RefreshToken, nil
}

// hwWriteBackRefresh 续期后把新凭据写回 vault（不动 dpop 私钥等其余字段）。
func hwWriteBackRefresh(file string, cred hwSTS, newRefresh string) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	acct, _ := v["account"].(map[string]any)
	sts, _ := acct["sts"].(map[string]any)
	if sts == nil {
		return fmt.Errorf("vault 结构异常（无 account.sts）")
	}
	sts["accessKeyId"] = cred.AccessKeyID
	sts["secretAccessKey"] = cred.SecretAccessKey
	sts["securityToken"] = cred.SecurityToken
	if newRefresh != "" {
		sts["refreshToken"] = newRefresh
	}
	// 关键：expiresAt 必须写新值。不写会导致下次读取仍判定「已过期」→ 再次续期，
	// 而 refresh_token 一次性，第二次续期直接失败并把账号锁死（必须重新登录）。
	if !cred.ExpiresAt.IsZero() {
		sts["expiresAt"] = cred.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	v["syncedAtMs"] = time.Now().UnixMilli()
	out, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := file + ".tmp-hw"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// hwDpopJWT DPoP 断言（ES256, r||s raw，低 S）。
func hwDpopJWT(priv *ecdsa.PrivateKey, htu, htm string) (string, error) {
	// 公钥 JWK
	xB := priv.PublicKey.X.FillBytes(make([]byte, 32))
	yB := priv.PublicKey.Y.FillBytes(make([]byte, 32))
	jwk := map[string]string{
		"kty": "EC", "crv": "P-256",
		"x": base64.RawURLEncoding.EncodeToString(xB),
		"y": base64.RawURLEncoding.EncodeToString(yB),
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	header := map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": jwk}
	payload := map[string]any{
		"htm": htm, "htu": htu,
		"iat": time.Now().Unix(),
		"jti": fmt.Sprintf("%x", jti),
	}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sum := sha256.Sum256([]byte(signing))
	// ASN.1 → raw r||s，低 S 归一化（P-256 阶的一半）
	r, s, err := ecdsa.Sign(rand.Reader, priv, sum[:])
	if err != nil {
		return "", err
	}
	n := priv.PublicKey.Curve.Params().N
	half := new(big.Int).Rsh(n, 1)
	if s.Cmp(half) > 0 {
		s.Sub(n, s)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

var _ = bytes.MinRead // 保险：保留 bytes 引用（WriteBack 用 os.WriteFile 即可）
