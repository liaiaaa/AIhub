package main

// 国际代理：WorkBuddy AI（--auth-dir 指定凭证目录）→ 本地 OpenAI 兼容路由。
// 对齐 converter.py：后端 www.workbuddy.ai，模型别名映射。

import "path/filepath"

func NewIntlProxy(authDir string) *Proxy {
	return &Proxy{
		Name:          "intl",
		Port:          IntlPort,
		Backend:       "https://www.workbuddy.ai",
		DefaultDomain: "www.workbuddy.ai",
		UserAgent:     "codebuddy-guoji/1.0",
		DefaultModel:  "deepseek-v4.1-flash",
		Models: []string{
			"deepseek-v4.1-flash",
			"hy4-preview",
			"glm-5.2",
			"glm-5.1",
			"kimi-k2.6",
			"minimax-m3",
			"auto",
			"gpt-5.4",
			"gpt-5.5",
			"gpt-5.6-luna",
			"gpt-5.6-terra",
			"gpt-5.6-sol",
			"gpt-6-astra",
		},
		Aliases: map[string]string{
			"deepseek-v4-flash":      "deepseek-v4.1-flash",
			"deepseek-v4.1-flash":    "deepseek-v4.1-flash",
			"deepseek-v4-flash-free": "deepseek-v4.1-flash",
			"Deepseek-V4.1-Flash":    "deepseek-v4.1-flash",
			"auto":                   "auto",
		},
		AuthDir:       authDir,
		AuthFileName:  "workbuddy-desktop-ai.info",
		PrependSystem: true,
		Owner:         "workbuddy-ai",
		LogPath:       "converter-intl.log",
	}
}

func credDirs() map[string]string {
	return map[string]string{
		"<凭证目录A>":  dataPath("<凭证目录A>"),
		"<凭证目录B>": dataPath("<凭证目录B>"),
		"<凭证目录C>":  dataPath("<凭证目录C>"),
	}
}

// dirHasCredential 判断凭证目录里是否有可用的 .info 文件。
// <凭证目录A>/<凭证目录B> 用标准名 workbuddy-desktop-ai.info；
// <凭证目录C>里的文件名是 ai-<uuid>.info（uuid 因账号而异），不能按名硬编码检查。
func dirHasCredential(dir string) bool {
	if fileExists(filepath.Join(dir, "workbuddy-desktop-ai.info")) {
		return true
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.info"))
	for _, f := range m {
		if fileExists(f) { // 排除同名目录
			return true
		}
	}
	return false
}
