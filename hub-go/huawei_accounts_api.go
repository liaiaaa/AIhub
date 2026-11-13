package main

// 果办账号管理 HTTP API + 控制台路由（让 aihub 完全替代 dsh 插件）。
//
// 端点（都挂在控制台 8801 上，走 withConsoleGuard 的本机来源校验）：
//   GET  /api/huawei/accounts        账号列表（id/名称/凭据剩余/是否选中/可用性）
//   GET  /api/huawei/login/status    登录流程状态（前端轮询）
//   POST /api/huawei/login/start     发起登录，返回授权地址
//   POST /api/huawei/login/cancel    取消登录
//   POST /api/huawei/accounts/select 切换选中账号
//   POST /api/huawei/accounts/remove 删除账号
//   POST /api/huawei/accounts/test   测试账号（真打一次订阅端点）
//   POST /api/huawei/logout          删除全部账号（退出登录）
//
// 设计口径与项目既有约定一致：
//   - 状态变更只收 POST（withConsoleGuard 的 isConsoleWritePath 已覆盖 /login/、/accounts/ 后缀）
//   - 所有操作返回 {ok, msg, ...}，前端 toast 直接显示 msg
//   - 删除账号要二次确认（前端做），后端只校验 id 合法

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// hwAccountView 账号的展示视图。
type hwAccountView struct {
	ID         string `json:"id"`
	Label      string `json:"label"`     // domainId
	UserName   string `json:"user_name"` // account_name
	UserID     string `json:"user_id"`   // principal_id
	Selected   bool   `json:"selected"`
	Usable     bool   `json:"usable"`
	Renewable  bool   `json:"renewable"`
	STSRestMin int    `json:"sts_rest_min"` // STS 剩余分钟（负数=已过期）
	ExpiresAt  string `json:"expires_at"`
	Reason     string `json:"reason,omitempty"` // 不可用原因
	Origin     string `json:"origin"`           // login / import
}

// hwAccountsView 组装账号列表（不触发续期——列表要快，续期留给实际使用时）。
func hwAccountsView() []hwAccountView {
	accounts := hwLoadAccounts()
	sel := hwSelectedAccountID()
	out := make([]hwAccountView, 0, len(accounts))
	for i, a := range accounts {
		v := hwAccountView{
			ID:        a.id,
			Label:     a.label,
			UserName:  a.userName,
			UserID:    a.userID,
			Selected:  (sel != "" && a.id == sel) || (sel == "" && i == 0),
			Renewable: a.refresh != "" && a.priv != nil,
			Origin:    "login",
		}
		if !a.sts.ExpiresAt.IsZero() {
			v.ExpiresAt = a.sts.ExpiresAt.Local().Format("01-02 15:04")
			v.STSRestMin = int(time.Until(a.sts.ExpiresAt).Minutes())
		}
		switch {
		case a.sts.AccessKeyID == "":
			v.Reason = "凭据缺失"
		case v.STSRestMin < 0 && !v.Renewable:
			v.Reason = "已过期且无法续期（需重新登录）"
		default:
			v.Usable = true
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// apiHuaweiAccounts 账号列表。
func (h *Hub) apiHuaweiAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": hwAccountsView(),
		"selected": hwSelectedAccountID(),
		"vault":    hwVaultPath(),
		"login":    hwLoginState(),
	})
}

// apiHuaweiLoginStatus 登录状态轮询。
func (h *Hub) apiHuaweiLoginStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, hwLoginState())
}

// apiHuaweiLoginStart 发起登录。
func (h *Hub) apiHuaweiLoginStart(w http.ResponseWriter, r *http.Request) {
	authURL, err := hwLoginStart()
	if err != nil && authURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	// 自动开浏览器（与控制台其他「开页面」行为一致）
	openURLInBrowser(authURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"msg":           "已发起登录，请在弹出的浏览器里完成授权",
		"authorize_url": authURL,
	})
}

// apiHuaweiLoginCancel 取消登录。
func (h *Hub) apiHuaweiLoginCancel(w http.ResponseWriter, r *http.Request) {
	hwLoginCancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已取消登录"})
}

// apiHuaweiAccountsSelect 切换选中账号。
func (h *Hub) apiHuaweiAccountsSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if !hwValidAccountID(req.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号 id 不合法"})
		return
	}
	if _, err := os.Stat(filepath.Join(hwVaultPath(), req.ID+".json")); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号不存在"})
		return
	}
	hwSelectAccount(req.ID)
	// 换号后作废网关三要素缓存（appKey 是按账号下发的）
	hwGatewayInvalidate()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "已切换账号"})
}

// apiHuaweiAccountsRemove 删除账号。
func (h *Hub) apiHuaweiAccountsRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if !hwValidAccountID(req.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号 id 不合法"})
		return
	}
	file := filepath.Join(hwVaultPath(), req.ID+".json")
	if err := os.Remove(file); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "删除失败：" + err.Error()})
		return
	}
	hwGatewayInvalidate()
	logHub("果办：已删除账号 %s", req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "账号已删除"})
}

// apiHuaweiLogout 删除全部账号。
func (h *Hub) apiHuaweiLogout(w http.ResponseWriter, r *http.Request) {
	matches, _ := filepath.Glob(filepath.Join(hwVaultPath(), "acc_*.json"))
	n := 0
	for _, f := range matches {
		if os.Remove(f) == nil {
			n++
		}
	}
	os.Remove(filepath.Join(hwVaultPath(), "_selected.json"))
	hwGatewayInvalidate()
	logHub("果办：已退出登录，删除 %d 个账号", n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "msg": fmt.Sprintf("已退出登录（删除 %d 个账号）", n)})
}

// apiHuaweiAccountTest 测试账号：真打一次订阅端点验证凭据可用。
func (h *Hub) apiHuaweiAccountTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.ID != "" && !hwValidAccountID(req.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "账号 id 不合法"})
		return
	}
	cred, label, err := hwFreshSTS()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "凭据不可用：" + err.Error()})
		return
	}
	body, status, err := httpGetJSON(hwSubscriptionBase+"/v1/subscription",
		signHuaweiV11(cred, "GET", hwSubscriptionBase+"/v1/subscription", nil, nil))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "msg": "网络失败：" + err.Error()})
		return
	}
	if status != http.StatusOK {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "msg": fmt.Sprintf("HTTP %d：%s", status, truncate(string(body), 120)),
		})
		return
	}
	var j struct {
		SubscribeStatus string `json:"subscribe_status"`
	}
	json.Unmarshal(body, &j)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"msg":       fmt.Sprintf("凭据可用（%s · 订阅 %s）", truncate(label, 8), j.SubscribeStatus),
		"subscribe": j.SubscribeStatus,
	})
}
