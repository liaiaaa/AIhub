package main

// 国内代理：CodeBuddy/WorkBuddy 桌面端 → 本地 OpenAI 兼容路由。
// 对齐 converter_cn.py：后端 copilot.tencent.com，原生 function calling。

import (
	"os"
	"path/filepath"
	"strings"
)

// wbCNBase 国内 WorkBuddy 上游 base。
//
// 默认 https://www.workbuddy.cn —— 依据是**本机安装的官方国内版客户端**自己的
// 产品配置：`/home/<user>/opt/workbuddy/workbuddy_dom/app/resources/app.asar.unpacked/
// cli/product.json` 里 `"endpoint": "https://www.workbuddy.cn"`（officialEndpoints
// 里 copilot.tencent.com 只是历史候选之一）。官方客户端打哪个域，我们就打哪个域，
// 这比「猜」靠谱。
//
// 2026-11-05 实测：同一份 dom 凭证打新老两个域名的 /v3/config 都返回 34 个条目，
// 路径同构、可互换；copilot.tencent.com 对海外出口会直接复位 TLS 握手（社区
// linguo2625469/workbuddy2api-panel 的记录）。
//
// 回退旧域：AIHUB_WB_CN_BASE=https://copilot.tencent.com。
func wbCNBase() string {
	if v := strings.TrimSpace(os.Getenv("AIHUB_WB_CN_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://www.workbuddy.cn"
}

// NewDomProxy 传统解析路径（不指定凭证目录）。
func NewDomProxy() *Proxy { return NewDomProxyDir("") }

// NewDomProxyDir 指定凭证目录启动国内代理（2026-10-13 国内多凭证）。
// dir 为空时保持原行为：AuthFilePath 按传统顺序解析
// （WORKBUDDY_AUTH_DIR → 镜像目录 → 数据根 → 数据根任意子目录）。
func NewDomProxyDir(dir string) *Proxy {
	p := &Proxy{
		Name:          "dom",
		Port:          DomPort,
		Backend:       wbCNBase(),
		DefaultDomain: "www.codebuddy.cn",
		// 所有不经 officialChatHeaders 的国内请求（签到 / 探活 / 模型列表探测）
		// 都必须报官方身份。旧值 codebuddy2openai/2.0 是自报家门的字符串。
		UserAgent:    officialDomUA(),
		DefaultModel: "auto",
		Models: []string{
			"space-bunny", "glm-5.3", "glm-5.3-flash",
			"glm-5.2", "glm-5.1", "glm-5v-turbo",
			"kimi-k2.7", "kimi-k2.6", "kimi-k2.5",
			"deepseek-v4-pro", "deepseek-v4-flash",
			"minimax-m3-pay", "hy3-preview-agent",
			"step-5-preview", "auto",
		},
		Aliases:       map[string]string{},
		AuthFileName:  "workbuddy-desktop.info",
		PrependSystem: false,
		// 2026-10-20 23:00 起腾讯给对话网关加了第三方客户端渠道校验（11128），
		// 整套请求指纹按官方桌面端内置 CLI 对齐，见 officialChatHeaders。
		OfficialChatClient: true,
		// 上游对 system 里的「You are … powered by …」身份句做连续子串匹配，
		// 命中即 11128；转发前替换成中性表述。
		SanitizeHarness: true,
		// 客户端没指定思考档位时补最高档（2026-10-08 用户要求，见 default_thinking.go）。
		// 实测 copilot.tencent.com 对 glm-5.1/glm-5.3/deepseek-v4-flash/minimax-m3-pay/auto
		// 接受 reasoning_effort=max，不支持的模型会忽略该字段。
		ThinkingDefault: wbThinkingDefaultEffort,
		Owner:           "codebuddy",
		LogPath:         "converter-cn.log",
		DirectOnly:      true, // copilot.tencent.com 国内可达，直连不读系统代理
	}
	p.AuthDir = dir // 放在字面量外：dir 为空串时行为与原 NewDomProxy 完全一致
	return p
}

// DomAuthFilePath 供控制台展示：优先系统 auth 目录的国内登录文件。
func DomAuthFilePath() string {
	p := NewDomProxy()
	return p.AuthFilePath()
}

var _ = filepath.Join
