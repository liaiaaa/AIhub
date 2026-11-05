package main

// Qoder 多凭证看板（2026-11-05）。
//
// 用户反馈：线路页 Qoder 卡片里那个「下拉 + 切换 + 存快照」太丑，要求和
// WorkBuddy 的多凭证卡片一个样式。WorkBuddy 那套（dom/intl）是
// `cred-card` 单选卡片：昵称、状态徽章、余额、原因，点一下就换凭证。
//
// Qoder 的差别在于：桌面端只有**一个** auth.v1.dat，换号即覆盖（旧号凭据当场销毁，
// 见 qoder_vault.go 文件头）。所以这里的「凭证」= 快照仓库里的那几份，
// 「换凭证」= 把对应快照写回 auth.v1.dat + 重启线路。语义不同，UI 相同。
//
// 状态来源：
//   - 选中：快照内容与当前 auth.v1.dat 字节一致（qoderVaultFindByContent）；
//   - 昵称/uid/到期：meta.json（存快照时已解密写入，零成本）；
//   - 余额：用**该快照自己的 token** 打 /sash/api/v2/me/usage —— 与 dom 探活同性质，
//     会打上游，所以带 TTL 缓存，默认 90 秒，force=1 才重查。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	qoderCredsMu    sync.Mutex
	qoderCredsCache []intlCredStatus
	qoderCredsAt    time.Time
)

const qoderCredsTTL = 90 * time.Second

// qoderCredsUsageURL 额度查询地址。做成变量是为了能在测试里指向本地假上游
// （qoderOpenAPIBase 是常量，直接拼就没法测）。
var qoderCredsUsageURL = qoderOpenAPIBase + "/sash/api/v2/me/usage"

// qoderVaultTokenFromBytes 从快照字节解出 token（不解密成功就返回错误）。
func qoderVaultTokenFromBytes(b []byte) (qoderCreds, error) {
	var out qoderCreds
	plain, _, err := qoderOpenCreds(qoderAppDataDir(), b)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(plain, &out); err != nil {
		return out, err
	}
	if out.Token == "" {
		return out, fmt.Errorf("token 为空")
	}
	return out, nil
}

// qoderUsageWithToken 用指定 token 查额度（qoderCredits 的「按凭证」版本）。
func qoderUsageWithToken(token string) (*qoderUsagePayload, error) {
	headers := map[string]string{
		"Authorization":   "Bearer " + token,
		"Cosy-ClientType": "10",
		"User-Agent":      "Qoder",
		"Accept":          "application/json",
	}
	body, status, err := httpGetJSON(qoderCredsUsageURL, headers)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var envelope struct {
		QoderUsage *qoderUsagePayload `json:"qoderUsage"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.QoderUsage != nil {
		return envelope.QoderUsage, nil
	}
	var bare qoderUsagePayload
	if json.Unmarshal(body, &bare) == nil && bare.UserID != "" {
		return &bare, nil
	}
	return nil, fmt.Errorf("响应里没有额度数据")
}

// qoderCredsStatus 返回全部快照的看板状态（与 domCredsStatus 同一结构，前端复用）。
func qoderCredsStatus(force bool) []intlCredStatus {
	qoderCredsMu.Lock()
	if !force && len(qoderCredsCache) > 0 && time.Since(qoderCredsAt) < qoderCredsTTL {
		out := make([]intlCredStatus, len(qoderCredsCache))
		copy(out, qoderCredsCache)
		qoderCredsMu.Unlock()
		markQoderSelected(out)
		return out
	}
	qoderCredsMu.Unlock()

	list, err := qoderVaultList()
	if err != nil {
		return nil
	}
	out := make([]intlCredStatus, 0, len(list))
	for _, a := range list {
		st := intlCredStatus{
			Name:   a.Name,
			Nick:   a.Nick,
			UID:    a.UID,
			Remain: -1,
			State:  "未知",
		}
		if a.ExpiresAt != "" {
			// 注意：Until 是「限流恢复时刻」专用字段（前端会渲染成「限流到 HH:MM」），
			// token 到期日不能塞这里，否则卡片会误报成限流。写进 Reason。
			if t, terr := time.Parse(time.RFC3339, a.ExpiresAt); terr == nil {
				if time.Now().After(t) {
					st.State = "已过期"
					st.Reason = "token 已于 " + t.Format("2006-01-02") + " 到期，需在桌面端重新登录后重存快照"
				} else {
					st.Reason = "token 到 " + t.Format("01-02") + " 到期"
				}
			}
		}
		b, rerr := os.ReadFile(filepath.Join(qoderVaultRoot(), a.Name, "auth.v1.dat"))
		if rerr != nil {
			st.Reason = "快照文件缺失"
			out = append(out, st)
			continue
		}
		creds, derr := qoderVaultTokenFromBytes(b)
		if derr != nil {
			// 解不开 = 换过机器/密钥环，这份快照在这台机器上不可用
			if st.State != "已过期" {
				st.State = "解不开"
				st.Reason = "本机密钥解不开这份快照（换机器或重装后会出现），需重新登录"
			}
			out = append(out, st)
			continue
		}
		// 余额：只有没过期时才打上游，省请求
		if st.State != "已过期" {
			usage, uerr := qoderUsageWithToken(creds.Token)
			if uerr != nil {
				// 401/403 说明这张票已经不被上游接受（换号后旧 token 可能失效）
				st.State = "登录失效"
				st.Reason = "上游不接受这份快照的 token（" + uerr.Error() + "），需在桌面端重新登录后重存快照"
			} else {
				if remain, total, ok := qoderCreditsTotals(usage); ok {
					st.Remain, st.Total = remain, total
				}
				if st.Nick == "" {
					st.Nick = usage.UserID
				}
				if usage.IsQuotaExceeded {
					st.State = "额度用尽"
					st.Reason = "额度已用尽（remain 0）"
				} else {
					st.OK = true
					st.State = "可用"
				}
			}
		}
		out = append(out, st)
	}

	qoderCredsMu.Lock()
	qoderCredsCache, qoderCredsAt = out, time.Now()
	qoderCredsMu.Unlock()
	markQoderSelected(out)
	return out
}

// markQoderSelected 按「内容与当前 auth.v1.dat 相同」标记选中项。
func markQoderSelected(list []intlCredStatus) {
	live, err := os.ReadFile(filepath.Join(qoderAppDataDir(), "auth.v1.dat"))
	if err != nil {
		return
	}
	cur := qoderVaultFindByContent(live)
	for i := range list {
		list[i].Selected = list[i].Name == cur
	}
}

// qoderCredCurrentName 当前生效的快照名（没有则空）。
func qoderCredCurrentName() string {
	live, err := os.ReadFile(filepath.Join(qoderAppDataDir(), "auth.v1.dat"))
	if err != nil {
		return ""
	}
	return qoderVaultFindByContent(live)
}

// qoderCredSwitch 切到指定快照；线路在跑就重启（QoderProxy 的 token 在内存里）。
func (h *Hub) qoderCredSwitch(name string) (bool, string) {
	if name == "" {
		return false, "未指定凭证"
	}
	if _, err := os.Stat(filepath.Join(qoderVaultRoot(), name)); err != nil {
		return false, "未知凭证：" + name
	}
	msg, err := qoderVaultUse(name)
	if err != nil {
		return false, err.Error()
	}
	if h.qoderRunning() {
		h.stopQoder()
		ok, detail := h.startQoder()
		if !ok {
			return false, msg + "；但线路重启失败：" + detail
		}
		msg += "；线路已重启（" + detail + "）"
	} else {
		msg += "；线路未运行，启动时即用此凭证"
	}
	qoderInvalidateCredsCache()
	logHub("qoder 换凭证：%s", msg)
	return true, msg
}

// qoderInvalidateCredsCache 让下一次看板请求重新查上游。
func qoderInvalidateCredsCache() {
	qoderCredsMu.Lock()
	qoderCredsCache = nil
	qoderCredsAt = time.Time{}
	qoderCredsMu.Unlock()
}

// qoderCredsSortedNames 快照名升序（给状态接口用，稳定顺序）。
func qoderCredsSortedNames() []string {
	list, _ := qoderVaultList()
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}
