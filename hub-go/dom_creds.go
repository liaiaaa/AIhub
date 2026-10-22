package main

// 国内多凭证（2026-10-13 用户要求）：把国际线路的「多凭证目录」机制带给
// 国内 WorkBuddy/CodeBuddy。动机：CodeBuddy IDE 桌面端登录的账号与
// workbuddy-desktop.info（国内workbuddy凭证/）不是同一个账号，
// 两个账号 = 两份独立积分池，可像国际线路一样单选切换。
//
// 凭证来源：CodeBuddy IDE 的登录态存在 state.vscdb 的 safeStorage 密文里
// （v11 信封，主密钥在 GNOME 密钥环 application="CodeBuddy CN"），
// 已由外部脚本解密导出为标准 workbuddy-desktop.info 放进数据根的
// 「CodeBuddy凭证/」目录。refreshToken 一并保留，hub-go 可自行刷新，
// 与桌面端互不干扰（刷新端点按 X-Refresh-Token 轮换，见 auth.go）。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// domAuthFileName 国内线路凭证文件标准名。
const domAuthFileName = "workbuddy-desktop.info"

// 内置国内凭证目录名：即便目录暂时为空也出现在控制台列表里，
// 免得用户以为凭证丢了（与国际线路 credDirs() 同一口径）。
var builtinDomCreds = []string{"国内workbuddy凭证", "CodeBuddy凭证"}

// domCredDirs 返回国内线路可选的凭证目录：显示名 → 绝对路径。
//
// 扫描规则与国际线路 credDirs() 对齐（刻意收窄，避免误捡）：
//   - 只看数据根**下一级**目录；
//   - 目录里必须存在标准文件名 workbuddy-desktop.info；
//   - 因此国际凭证目录（cred-a*/cred-c/cred-d）不会被认领。
func domCredDirs() map[string]string {
	out := map[string]string{}
	for _, n := range builtinDomCreds {
		out[n] = dataPath(n)
	}
	entries, err := os.ReadDir(hubRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := out[name]; ok {
			continue // 内置的已加过
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		if fileExists(filepath.Join(hubRoot, name, domAuthFileName)) {
			out[name] = filepath.Join(hubRoot, name)
		}
	}
	return out
}

// domCredNames 固定顺序的国内凭证名单（内置的排前面，下拉/单选不跳动）。
func domCredNames() []string {
	dirs := domCredDirs()
	names := make([]string, 0, len(dirs))
	builtin := map[string]bool{}
	for _, n := range builtinDomCreds {
		if dirs[n] != "" {
			names = append(names, n)
		}
		builtin[n] = true
	}
	var extra []string
	for n := range dirs {
		if !builtin[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}

// domCredDir 按显示名取目录；空名或未知 → 空串（调用方走传统解析路径）。
func domCredDir(name string) string {
	if name == "" {
		return ""
	}
	return domCredDirs()[name]
}

// dirHasDomCredential 目录里是否有国内线路凭证文件。
func dirHasDomCredential(dir string) bool {
	return dir != "" && fileExists(filepath.Join(dir, domAuthFileName))
}
