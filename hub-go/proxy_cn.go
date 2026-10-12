package main

// 国内代理：CodeBuddy/WorkBuddy 桌面端 → 本地 OpenAI 兼容路由。
// 对齐 converter_cn.py：后端 copilot.tencent.com，原生 function calling。

import "path/filepath"

func NewDomProxy() *Proxy {
	return &Proxy{
		Name:          "dom",
		Port:          DomPort,
		Backend:       "https://copilot.tencent.com",
		DefaultDomain: "www.codebuddy.cn",
		UserAgent:     "codebuddy2openai/2.0",
		DefaultModel:  "auto",
		Models: []string{
			"space-bunny", "glm-5.3", "glm-5.3-flash",
			"glm-5.2", "glm-5.1", "glm-5v-turbo",
			"kimi-k2.7", "kimi-k2.6", "kimi-k2.5",
			"deepseek-v4-pro", "deepseek-v4-flash",
			"minimax-m3-pay", "hy3-preview-agent", "auto",
		},
		Aliases:       map[string]string{},
		AuthFileName:  "workbuddy-desktop.info",
		PrependSystem: false,
		// 客户端没指定思考档位时补最高档（2026-10-08 用户要求，见 default_thinking.go）。
		// 实测 copilot.tencent.com 对 glm-5.1/glm-5.3/deepseek-v4-flash/minimax-m3-pay/auto
		// 接受 reasoning_effort=max，不支持的模型会忽略该字段。
		ThinkingDefault: wbThinkingDefaultEffort,
		Owner:           "codebuddy",
		LogPath:         "converter-cn.log",
		DirectOnly:      true, // copilot.tencent.com 国内可达，直连不读系统代理
	}
}

// DomAuthFilePath 供控制台展示：优先系统 auth 目录的国内登录文件。
func DomAuthFilePath() string {
	p := NewDomProxy()
	return p.AuthFilePath()
}

var _ = filepath.Join
