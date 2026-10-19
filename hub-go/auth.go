package main

// 凭据管理：读 auth JSON 文件、token 过期自动刷新并原子回写。
// 移植自 converter.py / converter_cn.py 的 CredentialManager。
//
// 与 Python 版对齐的两个关键点（Go 版早期实现有偏差，会引发线上问题）：
//  1) 刷新期间的网络请求【不持有】c.mu。否则控制台每 2 秒一次的 /api/status
//     会被卡住，进而占住 hub 的全局锁，导致「停止 / 退出」全部点不动。
//  2) 回写时保留原文件里的未知字段（accounts / allAccounts 等）。
//     桌面端把多账号信息存在这些字段里，早期版本按结构体序列化会把它们抹掉。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

type AuthData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	Domain           string `json:"domain"`
	ExpiresAt        int64  `json:"expiresAt"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	LastRefreshTime  int64  `json:"lastRefreshTime"`
}

type AccountData struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	EnterpriseID   string `json:"enterpriseId"`
	EnterpriseName string `json:"enterpriseName"`
}

type SessionFile struct {
	Auth    *AuthData    `json:"auth"`
	Account *AccountData `json:"account"`
}

type CredentialManager struct {
	path          string
	backend       string
	defaultDomain string
	userAgent     string

	// mu 只保护内存缓存；任何网络调用都不得在持锁期间进行。
	mu     sync.Mutex
	cached *SessionFile
	raw    map[string]any
	mtime  time.Time

	// refreshMu 串行化刷新，避免并发请求同时刷新同一个 token。
	refreshMu sync.Mutex

	// wbMeta 记录 auth 文件里哪些字段是 WorkBuddy 5.7.6 的加密信封
	// （见 workbuddy_atrest.go）。刷新回写时按原样再封回去，
	// 免得把桌面端的文件写成明文。
	wbMeta map[string]wbFieldMeta
}

func NewCredentialManager(path, backend, defaultDomain, userAgent string) *CredentialManager {
	return &CredentialManager{path: path, backend: backend, defaultDomain: defaultDomain, userAgent: userAgent}
}

// load 读取并缓存 auth 文件。调用方必须持有 c.mu。
// 返回解析后的结构体，以及原始 JSON（保留未知字段，供回写使用）。
func (c *CredentialManager) load() (*SessionFile, map[string]any, error) {
	st, err := os.Stat(c.path)
	if err != nil {
		return nil, nil, fmt.Errorf("无法读取 auth 文件：%v", err)
	}
	if c.cached == nil || !st.ModTime().Equal(c.mtime) {
		b, err := os.ReadFile(c.path)
		if err != nil {
			return nil, nil, err
		}
		// WorkBuddy 桌面端 5.7.6 起把凭据字段写成 AES-GCM 信封，
		// 直接 Unmarshal 会报 “cannot unmarshal object into ... nickname of type string”。
		// 这里先就地解封（国际线路的明文快照会原样返回）。
		plain, meta, err := wbUnsealBytes(b)
		if err != nil {
			return nil, nil, err
		}
		var s SessionFile
		if err := json.Unmarshal(plain, &s); err != nil {
			if meta != nil {
				return nil, nil, fmt.Errorf("auth 文件解密后仍解析失败：%v", err)
			}
			return nil, nil, fmt.Errorf("auth 文件解析失败：%v", err)
		}
		// UseNumber：expiresAt 是毫秒时间戳，用 float64 解析再写回会变成科学计数法。
		// 基底用**原始**字节，保住信封，回写时才不会再退回明文。
		raw := map[string]any{}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&raw); err != nil {
			raw = map[string]any{}
		}
		c.cached, c.raw, c.mtime, c.wbMeta = &s, raw, st.ModTime(), meta
	}
	if c.cached == nil {
		return nil, nil, fmt.Errorf("无法读取 auth 文件：%s", c.path)
	}
	return c.cached, c.raw, nil
}

// wbMetaSnapshot 在锁内取信封位置表：refresh() 全程不持有 c.mu，不能直接读字段。
func (c *CredentialManager) wbMetaSnapshot() map[string]wbFieldMeta {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wbMeta
}

// session 是 load 的加锁封装，只在读内存/文件时短暂持锁。
func (c *CredentialManager) session() (*SessionFile, map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load()
}

func (c *CredentialManager) isExpired(s *SessionFile) bool {
	if s == nil || s.Auth == nil {
		return true
	}
	return time.Now().UnixMilli() >= s.Auth.ExpiresAt-60_000
}

// Headers 返回带最新 token 的后端请求头；必要时先刷新。
func (c *CredentialManager) Headers() (map[string]string, error) {
	s, _, err := c.session()
	if err != nil {
		return nil, err
	}
	if s.Auth == nil {
		return nil, fmt.Errorf("auth 数据缺失")
	}
	if c.isExpired(s) {
		if err := c.refreshIfNeeded(); err != nil {
			return nil, err
		}
		if s, _, err = c.session(); err != nil {
			return nil, err
		}
		if s.Auth == nil {
			return nil, fmt.Errorf("auth 数据缺失")
		}
	}
	return c.buildHeaders(s.Auth, s.Account), nil
}

// refreshIfNeeded 在 token 过期时刷新。整个网络请求过程不持有 c.mu，
// 因此控制台的状态轮询、停止、退出都不会被刷新阻塞。
func (c *CredentialManager) refreshIfNeeded() error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	s, raw, err := c.session()
	if err != nil {
		return err
	}
	if !c.isExpired(s) {
		return nil // 并发请求已经刷新过了
	}
	return c.refresh(s, raw)
}

// refresh 调后端刷新 token 并原子回写 auth 文件（保留未知字段）。
func (c *CredentialManager) refresh(s *SessionFile, raw map[string]any) error {
	if s == nil || s.Auth == nil {
		return fmt.Errorf("auth 数据缺失")
	}
	auth := s.Auth
	headers := c.buildHeaders(auth, s.Account)
	headers["X-Refresh-Token"] = auth.RefreshToken
	headers["X-Auth-Refresh-Source"] = "plugin"
	url := c.backend + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := backendClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("刷新 token 网络失败：%v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("刷新 token 响应解析失败：%v", err)
	}
	if out.Code != 0 || len(out.Data) == 0 {
		return fmt.Errorf("刷新 token 失败：%s", out.Msg)
	}
	var na AuthData
	if err := json.Unmarshal(out.Data, &na); err != nil {
		return fmt.Errorf("刷新 token 数据解析失败：%v", err)
	}
	if na.Domain == "" {
		na.Domain = auth.Domain
	}
	if na.Domain == "" {
		na.Domain = c.defaultDomain
	}
	na.LastRefreshTime = time.Now().UnixMilli()
	if na.ExpiresAt == 0 && na.ExpiresIn > 0 {
		na.ExpiresAt = time.Now().UnixMilli() + na.ExpiresIn*1000
	}
	if na.RefreshExpiresAt == 0 && na.RefreshExpiresIn > 0 {
		na.RefreshExpiresAt = time.Now().UnixMilli() + na.RefreshExpiresIn*1000
	}

	// 组装回写内容：以原文件为基底，只替换 auth，
	// accounts / allAccounts 等桌面端字段原样保留。
	// 刷新期间桌面端可能改过文件，这里重读一次以拿到最新的未知字段。
	if fresh, err := os.ReadFile(c.path); err == nil {
		freshRaw := map[string]any{}
		dec := json.NewDecoder(bytes.NewReader(fresh))
		dec.UseNumber()
		if dec.Decode(&freshRaw) == nil {
			raw = freshRaw
		}
	}
	if len(raw) == 0 {
		raw = map[string]any{}
	}
	if _, ok := raw["account"]; !ok && s.Account != nil {
		var am map[string]any
		if b, err := json.Marshal(s.Account); err == nil {
			if json.Unmarshal(b, &am) == nil {
				raw["account"] = am
			}
		}
	}
	newAuth := map[string]any{}
	if prev, ok := raw["auth"].(map[string]any); ok {
		for k, v := range prev {
			newAuth[k] = v
		}
	}
	// 用结构体序列化保证数值类型正确，再覆盖到基底上。
	nb, err := json.Marshal(na)
	if err != nil {
		return err
	}
	var nm map[string]any
	dec := json.NewDecoder(bytes.NewReader(nb))
	dec.UseNumber()
	if err := dec.Decode(&nm); err != nil {
		return err
	}
	for k, v := range nm {
		newAuth[k] = v
	}
	raw["auth"] = newAuth

	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	// 桌面端 5.7.6 起把凭据加密存放。刷新拿到的是明文 token，
	// 回写前必须重新封成它的信封格式，否则会把它的文件降级成明文，
	// 下一版客户端可能拒绝读（它自己的读路径按 fields 策略校验）。
	if meta := c.wbMetaSnapshot(); len(meta) > 0 {
		re, err := wbResealBytes(b, meta)
		if err != nil {
			return fmt.Errorf("凭据已刷新但回写失败（重新加密失败，内存里的新 token 仍可用）：%v", err)
		}
		b = re
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return err
	}

	// 写入新对象而不是原地改 s.Auth：Summary() 可能正在并发读取旧对象。
	ns := &SessionFile{Auth: &na, Account: s.Account}
	c.mu.Lock()
	c.cached, c.raw = ns, raw
	if st, err := os.Stat(c.path); err == nil {
		c.mtime = st.ModTime()
	}
	c.mu.Unlock()
	return nil
}

func (c *CredentialManager) buildHeaders(auth *AuthData, acct *AccountData) map[string]string {
	if auth == nil {
		return map[string]string{}
	}
	domain := auth.Domain
	if domain == "" {
		domain = c.defaultDomain
	}
	// acct 可能为 nil（auth 文件没有 account 段），早期版本在这里空指针 panic。
	uid, ent := "", ""
	if acct != nil {
		uid, ent = acct.UID, acct.EnterpriseID
	}
	return map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "application/json",
		"Authorization":   "Bearer " + auth.AccessToken,
		"X-User-Id":       uid,
		"X-Enterprise-Id": ent,
		"X-Tenant-Id":     ent,
		"X-Domain":        domain,
		"User-Agent":      c.userAgent,
	}
}

// Summary 返回账号摘要（用于 /health 与控制台展示）。只读缓存，不做网络调用。
func (c *CredentialManager) Summary() (map[string]any, error) {
	s, _, err := c.session()
	if err != nil {
		return nil, err
	}
	nick, uid, ent := "", "", ""
	if s.Account != nil {
		nick, uid, ent = s.Account.Nickname, s.Account.UID, s.Account.EnterpriseName
	}
	exp, expired := int64(0), true
	if s.Auth != nil {
		exp = s.Auth.ExpiresAt
		expired = c.isExpired(s)
	}
	return map[string]any{
		"uid":              uid,
		"nickname":         nick,
		"enterpriseName":   ent,
		"token_expires_at": exp,
		"token_expired":    expired,
	}, nil
}
