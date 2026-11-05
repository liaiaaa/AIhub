package main

// 桌面端「实时凭据」同步（2026-11-05）。
//
// 背景：`国内workbuddy凭证/workbuddy-desktop.info` 是 2026-10-08 手工导出的一份副本，
// 里面是 **console 身份** 的 token（JWT azp=console）。目录 /v3/config 还认它，
// 但聊天网关已经只认桌面端会话，于是控制台「探活」稳定 401（openresty/APISIX 的
// 401 Authorization Required），而用户在同一台机器的官方客户端里用得好好的。
//
// 官方 Linux 客户端自己会把**实时**凭据写到：
//
//	~/.local/share/CodeBuddyExtension/Data/Public/auth/workbuddy-desktop.info
//
// 字段是 $wbEncrypted 信封，密钥就是 hub 已有的 wbatrest.key（见 workbuddy_atrest.go），
// 所以 hub 能直接读、能解封 —— 2026-11-05 实测：用这份实时文件做探活是 result=0（成功）。
//
// 因此这里做一层「只读同步」：每次读凭据前，如果桌面端文件比 hub 副本新，
// 就原子地覆盖副本。hub 自己的 refresh 仍然写副本（不碰桌面端文件），
// 桌面端下次刷新后再覆盖回来 —— 双方互不干扰，且 hub 永远拿到最新那张票。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// wbLiveCredRelPath 桌面端实时凭据相对 $HOME 的路径（Linux 版 WorkBuddy 实测位置）。
const wbLiveCredRelPath = ".local/share/CodeBuddyExtension/Data/Public/auth/workbuddy-desktop.info"

// wbLiveCredSource 返回桌面端实时凭据的源路径；不存在则返回空串。
//
//	AIHUB_WB_LIVE_CRED=/path/to/workbuddy-desktop.info 可覆盖
//	AIHUB_WB_LIVE_CRED=off 关闭同步
func wbLiveCredSource() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_WB_LIVE_CRED")); v != "" {
		if strings.EqualFold(v, "off") || strings.EqualFold(v, "0") {
			return ""
		}
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	p := filepath.Join(home, wbLiveCredRelPath)
	if !fileExists(p) {
		return ""
	}
	return p
}

// wbLiveCredDest 返回 hub 侧要同步到的副本路径。
//
//	AIHUB_WB_LIVE_DEST=/dir 或 /dir/workbuddy-desktop.info 可覆盖
//	默认是数据根下的「国内workbuddy凭证/workbuddy-desktop.info」
func wbLiveCredDest() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_WB_LIVE_DEST")); v != "" {
		if st, err := os.Stat(v); err == nil && st.IsDir() {
			return filepath.Join(v, domAuthFileName)
		}
		return v
	}
	return filepath.Join(dataPath("国内workbuddy凭证"), domAuthFileName)
}

// syncDomLiveCred 在读取凭据前把桌面端的实时文件同步到副本。
//
// 只对「副本路径」生效：其它凭证目录（其它凭证目录 …）各是各的账号，
// 不能被同一个桌面端登录态覆盖。调用方必须持有 c.mu。
func (c *CredentialManager) syncDomLiveCred() {
	src := wbLiveCredSource()
	if src == "" {
		return
	}
	dst := wbLiveCredDest()
	if filepath.Clean(c.path) != filepath.Clean(dst) {
		return
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		return // 直接指向桌面端文件时无需同步
	}
	sst, err := os.Stat(src)
	if err != nil || sst.IsDir() {
		return
	}
	if dstSt, derr := os.Stat(dst); derr == nil && !sst.ModTime().After(dstSt.ModTime()) {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	// 只接受「能解封 + 是完整 JSON + 有 auth」的文件：桌面端可能正在写，
	// 读到半截时宁可不覆盖（wbUnsealBytes 对无信封的明文不做语法校验，得自己补）。
	plain, _, err := wbUnsealBytes(b)
	if err != nil {
		return
	}
	var probe struct {
		Auth json.RawMessage `json:"auth"`
	}
	if err := json.Unmarshal(plain, &probe); err != nil || len(probe.Auth) == 0 {
		return
	}
	tmp := dst + ".tmp-livesync"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
	}
}
