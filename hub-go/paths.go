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
//  1. 环境变量 AIHUB_ROOT —— 显式覆盖，便于测试与多环境部署
//  2. exe 所在目录的上一级 —— 兼容当前部署（exe 在 hub-go\，数据在仓库根）
//  3. exe 所在目录 —— exe 已挪到 bin\ 等子目录、数据就在同级时
//  4. 当前工作目录 —— 兜底（例如 go run）
//
// 注意：解析结果会在候选目录中挑选「看起来像数据根」的那个（见 looksLikeDataRoot），
// 保证即使 exe 被移动，也不会把日志/状态写到一个随机的空目录里。

import (
	"os"
	"strings"
	"path/filepath"
)

// dataRootEnv 显式指定数据根目录的环境变量名。
const dataRootEnv = "AIHUB_ROOT"

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

	// 从 exe 所在目录开始【逐级向上】查找真正的数据根，最多 4 级。
	// 需要向上多级是因为 exe 可能被放在 bin\ 这类子目录里：
	//   <root>\hub-go\bin\aihub.exe  -> 数据根在 <root>（向上 3 级）
	// 先近后远，保证「数据就在 exe 旁边」的部署仍然优先命中。
	candidates := []string{exeDir}
	dir := exeDir
	for i := 0; i < 4; i++ {
		dir = filepath.Dir(dir)
		if dir == "" || dir == "." || dir == filepath.Dir(dir) {
			break // 到根了
		}
		candidates = append(candidates, dir)
	}
	// 最后再考虑当前工作目录（例如 go run）。
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}

	for _, c := range candidates {
		if looksLikeDataRoot(c) {
			return c
		}
	}
	// 都不像时，保持历史行为：exe 的上一级。
	return filepath.Dir(exeDir)
}

// looksLikeDataRoot 判断目录是否像本项目的数据根。
//
// 只用「权威标志」——即 hub 自己不会在源码目录里生成的东西：
//   - state.json（hub 偏好，只在数据根写入）
//   - 任一已知凭证目录（<凭证目录A> / <凭证目录B> / <凭证目录C>，由用户放置）
//
// 刻意【不】把 converter-*.log / hub.log 当作标志：
// 它们是 hub 自己写出来的，一旦某次把日志写进了某个目录，
// 该目录下次就会被误判成数据根，错误会自我固化、再也回不到真正的根。
// （这是实际发生过的 bug：exe 在 bin\ 时，候选 hub-go\ 因残留日志被误判为数据根，
//
//	于是日志一直写在 hub-go\ 而不是仓库根。）
func looksLikeDataRoot(dir string) bool {
	if dir == "" {
		return false
	}
	markers := []string{
		"state.json",
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	// 本程序的数据根里通常有若干「<名字>凭证/」目录（用户在控制台建的账号
	// 目录）。2026-10-21 开源审查：原来这里写死作者本机的三个目录名，
	// 既泄露个人称呼，又对陌生人的目录名完全失效。改成「有任意 *凭证
	// 子目录」也算数据根标志，与 .gitignore 里 `*凭证/` 的口径一致。
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), "凭证") {
				return true
			}
		}
	}
	return false
}

// dataPath 返回数据根下的一个路径。所有落盘（日志/状态）都应经过它，
// 便于将来统一改动数据目录位置。
func dataPath(elem ...string) string {
	return filepath.Join(append([]string{hubRoot}, elem...)...)
}
