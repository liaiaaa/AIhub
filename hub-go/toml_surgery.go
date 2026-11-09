package main

// TOML 文本手术的辅助函数（2026-11-09 A′ 审查修复）。
//
// 为什么需要这些：takeOverCodex / restoreCodex 用逐行文本手术改 ~/.codex/config.toml，
// 而 TOML 的顶层键与 section 键、以及 `model` 与 `model_context_window` 这类前缀相同的键
// 都靠行文本很难分清。这里把它们收敛成几个小函数，并统一加 tomllib 校验兜底。

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tomlSectionName 返回该行的 section 名（`[a.b]` / `[[a.b]]` 都归一成 `a.b`），
// 不是 section 头则返回空串。允许行尾注释：`[model_providers.aihub] # 已有`。
func tomlSectionName(line string) string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return ""
	}
	// 找到配对的右方括号，后面只允许空白或注释
	depth := 0
	end := -1
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return ""
	}
	rest := strings.TrimSpace(t[end+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return "" // 方括号后面还有别的东西，不是合法 section 头
	}
	name := strings.Trim(t[:end+1], "[]")
	return strings.TrimSpace(name)
}

// tomlTopKey 解析顶层键值行，返回键名与值（值保留原样，含引号）。
// 不是 `key = value` 形式（空行、注释、数组元素、多行字符串续行）返回 ok=false。
func tomlTopKey(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return "", "", false
	}
	i := strings.Index(t, "=")
	if i <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(t[:i])
	// 键名只允许字母数字下划线连字符（TOML bare key），否则不是键值行
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", "", false
		}
	}
	return key, strings.TrimSpace(t[i+1:]), true
}

// topLevelHasKey 判断顶层区（第一个 section 之前）是否已有该键。
// 只看顶层，避免 profile / 注释里的同名键造成误判。
func topLevelHasKey(src, want string) bool {
	for _, ln := range strings.Split(src, "\n") {
		if tomlSectionName(ln) != "" {
			return false // 顶层区结束
		}
		if k, _, ok := tomlTopKey(ln); ok && k == want {
			return true
		}
	}
	return false
}

// tomlFileMode 取文件当前权限，取不到用 0600。
// config.toml 可能含 [shell_environment_policy.set] 里的 token，不许被放宽成 0644。
func tomlFileMode(path string) os.FileMode {
	if st, err := os.Stat(path); err == nil {
		return st.Mode().Perm()
	}
	return 0600
}

// tomlValid 用 python3 tomllib 校验内容是否为合法 TOML。
// 用 python3 而不是引入 Go 的 TOML 库：仓库零第三方依赖是硬约束。
// python3 不存在时返回 true（不阻塞；此时只剩 json/结构自检）。
func tomlValid(content []byte) (bool, string) {
	py, err := exec.LookPath("python3")
	if err != nil {
		return true, "python3 不可用，跳过 TOML 校验"
	}
	cmd := execCmd(py, "-c", "import sys,tomllib\ntry:\n tomllib.loads(sys.stdin.read())\nexcept Exception as e:\n print(e); sys.exit(3)\n")
	cmd.Stdin = bytes.NewReader(content)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return false, strings.TrimSpace(errBuf.String())
	}
	return true, ""
}

// writeFileChecked 原子写文件，写后用 tomllib 校验；校验失败自动从 bak 还原。
// mode 沿用调用方给的原权限。返回错误时磁盘上要么是原文（已还原），要么未被改动。
func writeFileChecked(path string, content []byte, mode os.FileMode, bak string) error {
	if mode == 0 {
		mode = 0600
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	ok, detail := tomlValid(content)
	if !ok {
		_ = os.Remove(tmp)
		if bak != "" {
			if b, rerr := os.ReadFile(bak); rerr == nil {
				if werr := os.WriteFile(path, b, mode); werr == nil {
					return &tomlCheckError{detail: detail, restored: filepath.Base(bak)}
				}
			}
		}
		return &tomlCheckError{detail: detail}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// tomlCheckError 校验失败的错误类型（带是否已还原的信息，便于回执里说清楚）。
type tomlCheckError struct {
	detail   string
	restored string
}

func (e *tomlCheckError) Error() string {
	if e.restored != "" {
		return "生成的 config.toml 不是合法 TOML（" + e.detail + "），已从备份 " + e.restored + " 还原"
	}
	return "生成的 config.toml 不是合法 TOML（" + e.detail + "），未写入"
}

// takeoverMarkerPath 记录「本次接管用的是哪个备份」，供 restore 优先使用。
func takeoverMarkerPath(configPath string) string { return configPath + ".aihub-backup" }

// preferredBackup 还原时的备份来源：先看 marker（本次接管留的），
// 没有再退回最新的 bak-hubgo-*。只按时间戳取最新会拿到一个月前的旧备份。
func preferredBackup(configPath string) string {
	if b, err := os.ReadFile(takeoverMarkerPath(configPath)); err == nil {
		p := strings.TrimSpace(string(b))
		if p != "" {
			if _, serr := os.Stat(p); serr == nil {
				return p
			}
			// marker 指向的备份没了：退回时间戳，但记一条日志便于排查
			logHub("接管备份 %s 已不存在，退回按时间戳查找", p)
		}
	}
	return latestBackup(configPath, "hubgo")
}

// topLevelValue 取顶层区某个键的值（去掉引号）；没有返回空串。
// 与 topLevelHasKey 一样只看顶层，避免 profile 段里的同名键造成误判。
func topLevelValue(src, want string) string {
	for _, ln := range strings.Split(src, "\n") {
		if tomlSectionName(ln) != "" {
			return ""
		}
		if k, v, ok := tomlTopKey(ln); ok && k == want {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
