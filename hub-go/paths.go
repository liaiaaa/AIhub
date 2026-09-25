package main

// 路径解析：数据根目录（hubRoot）。
//
// 历史上 hubRoot 直接写成 `filepath.Dir(filepath.Dir(exe))`，
// 把「数据放哪里」和「exe 放哪里」死死绑在一起：
// exe 在 hub-go\ 下时，数据根恰好是仓库根，看起来能用；
// 但只要把 exe 挪到 bin\、或者换一个构建输出目录，凭证/state/日志就会静默失联
// （表现为「国际代理起不来」这类难以定位的问题）。
//
// 现在的解析顺序（先命中先用）：
//  1. 环境变量 WORKBUDDY_HUB_ROOT —— 显式覆盖，便于测试与多环境部署
//  2. exe 所在目录的上一级 —— 兼容当前部署（exe 在 hub-go\，数据在仓库根）
//  3. exe 所在目录 —— exe 已挪到 bin\ 等子目录、数据就在同级时
//  4. 当前工作目录 —— 兜底（例如 go run）
//
// 注意：解析结果会在候选目录中挑选「看起来像数据根」的那个（见 looksLikeDataRoot），
// 保证即使 exe 被移动，也不会把日志/状态写到一个随机的空目录里。

import (
	"os"
	"path/filepath"
)

// dataRootEnv 显式指定数据根目录的环境变量名。
const dataRootEnv = "WORKBUDDY_HUB_ROOT"

// hubRoot 是数据根目录：凭证目录、state.json、各代理日志都相对于它解析。
// 它是一个变量（而非常量），以便测试与显式配置覆盖。
var hubRoot = resolveHubRoot()

// resolveHubRoot 按上述优先级推导数据根目录。
func resolveHubRoot() string {
	// 1) 显式覆盖优先
	if v := os.Getenv(dataRootEnv); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}

	exe, err := os.Executable()
	if err != nil {
		// 拿不到 exe 路径（极少见）：退回工作目录
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	exeDir := filepath.Dir(exe)

	// 候选按「更可能是数据根」排序。
	parent := filepath.Dir(exeDir)
	candidates := []string{parent, exeDir}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}

	for _, c := range candidates {
		if looksLikeDataRoot(c) {
			return c
		}
	}
	// 都不像时，保持历史行为：exe 的上一级。
	return parent
}

// looksLikeDataRoot 判断目录是否像本项目的数据根。
// 依据是数据根里的标志性内容（任一命中即可）：
//   - state.json（hub 偏好）
//   - 任一已知凭证目录（<凭证目录A> / <凭证目录B> / <凭证目录C>）
//   - 任一已知代理日志
//
// 这样在 exe 被移动到 bin\ 之后，仍能自动指回真正的数据根，
// 而不是把日志写进 bin\。
func looksLikeDataRoot(dir string) bool {
	if dir == "" {
		return false
	}
	markers := []string{
		"state.json",
		"<凭证目录A>", "<凭证目录B>", "<凭证目录C>",
		"converter-cn.log", "converter-intl.log",
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// dataPath 返回数据根下的一个路径。所有落盘（日志/状态）都应经过它，
// 便于将来统一改动数据目录位置。
func dataPath(elem ...string) string {
	return filepath.Join(append([]string{hubRoot}, elem...)...)
}
