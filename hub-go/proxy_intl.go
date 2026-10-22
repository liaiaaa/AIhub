package main

// 国际代理：WorkBuddy AI（--auth-dir 指定凭证目录）→ 本地 OpenAI 兼容路由。
// 对齐 converter.py：后端 www.workbuddy.ai，模型别名映射。

import (
	"os"
	"path/filepath"
	"strings"
)

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
			// 2026-10-15 补：倍率/workbuddy国际.md 里列有、但上游 /v1/models 目录
			// 不返回的模型。全部经 :8799 直连实测可用后才录入：
			//   step-5-preview / kimi-k3 / kimi-k2.8-preview / grok-4.7 /
			//   gemini-3.8-flash / gemini-3.5-flash / gpt-6-luna  → 均 200
			// 实测不可用故未录入：hy3-preview（400 11102）、gpt-6.1-sol（400 11102）、
			// gpt-6-sol（假 200：finish_reason=error、usage 全 0）。
			// 本地目录只影响 /v1/models 候选单与控制台探测，路由按模型名原样透传。
			"step-5-preview",
			"kimi-k3",
			"kimi-k2.8-preview",
			"grok-4.7",
			"gemini-3.8-flash",
			"gemini-3.5-flash",
			"gpt-6-luna",
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
		// 2026-10-16 起上游对 /v2/chat/completions 开通路校验，缺会话四头整条线路报
		// 11128「conversation id missing」。见 withConversationHeaders。
		ConversationHeaders: true,
		// 同国内线路：客户端没指定思考档位时补最高档（见 default_thinking.go）。
		ThinkingDefault: wbThinkingDefaultEffort,
		Owner:           "workbuddy-ai",
		LogPath:         "converter-intl.log",
	}
}

// credDirs 返回国际线路可选的凭证目录：目录名 → 绝对路径。
//
// 2026-10-12 起改为「内置三个 + 数据根下自动扫描」，不再硬编码写死：
// 用户每加一个账号就得改一次源码，是个明显的短板（这次加「<凭证目录D>」就撞上了）。
//
// 扫描规则（刻意收窄，避免误捡）：
//   - 只看数据根**下一级**目录；
//   - 目录里必须存在**国际线路的标准文件名** workbuddy-desktop-ai.info
//     或 ai-<uuid>.info（后者是账号不同导致的文件名差异，见 dirHasCredential）；
//   - 因此「国内workbuddy凭证/」这种放 workbuddy-desktop.info 的目录
//     **不会**被国际线路认领（那是国内线路的凭证，两套后端两套 token）。
//
// 2026-10-21 开源审查：这里原来硬编码作者本机的三个凭证目录名（个人称呼），
// 开源后会公开这些称呼，而且陌生人的目录名完全不同。改为完全不写死 ——
// 下面的目录扫描会把数据根下所有含国际凭证文件的目录都认领进来，
// 用户建什么名字都行。
//
// 代价：以前「目录暂时为空也出现在控制台列表里，免得用户以为凭证丢了」，
// 现在空目录不显示。这是正确的取舍：陌生人第一次启动时不该看到三个
// 指向不存在路径的空条目。
func credDirs() map[string]string {
	out := map[string]string{}
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
		if !strings.HasPrefix(name, ".") && intlCredDirHasFile(filepath.Join(hubRoot, name)) {
			out[name] = filepath.Join(hubRoot, name)
		}
	}
	return out
}

// intlCredDirHasFile 判断目录里是否有「国际线路」的凭证文件。
// 只认国际线路的文件名，不认 workbuddy-desktop.info（那是国内线路的）。
func intlCredDirHasFile(dir string) bool {
	if fileExists(filepath.Join(dir, "workbuddy-desktop-ai.info")) {
		return true
	}
	m, _ := filepath.Glob(filepath.Join(dir, "ai-*.info"))
	for _, f := range m {
		if fileExists(f) { // 排除同名目录
			return true
		}
	}
	return false
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
