package main

// MiMo 代理：小米 MiMo 桌面端（免费额度）→ 本地 OpenAI 兼容路由。
//
// 与国内/国际代理的区别：
//   - 认证不是 JSON 凭证文件，而是小米账号 SSO 换来的 serviceToken（见 auth_mimo.go）
//   - 后端聊天路径是 /route/chat/completions（不是 /v2/chat/completions）
//   - 后端基址带 /api 前缀
//
// 模型换代（2026-09-22）：小米已下架 preview 系模型，换成 V2.6。
// ID 以桌面端 %APPDATA%\Xiaomi MiMo\model-catalog.json 为准；
// 旧 preview 名保留为别名，旧客户端配置不用改。

const MimoPort = 8798

func NewMimoProxy() *Proxy {
	return &Proxy{
		Name:         "mimo",
		Kind:         "mimo",
		Port:         MimoPort,
		Backend:      "https://mimo-server-cn.xiaomimimo.com/api",
		ChatPath:     "/route/chat/completions",
		UserAgent:    "MiClaw/1.0",
		DefaultModel: "mimo-v2.6-flash",
		Models: []string{
			"mimo-v2.6-flash",
			"mimo-v2.6-pro",
			"mimo-v2.6-pro-ultraspeed",
		},
		// 桌面端的显示名也认，方便直接粘贴；旧 preview 名映射到对应新模型
		Aliases: map[string]string{
			"MiMo V2.6 Flash":          "mimo-v2.6-flash",
			"MiMo V2.6 Pro":            "mimo-v2.6-pro",
			"MiMo V2.6 Pro Ultraspeed": "mimo-v2.6-pro-ultraspeed",
			"mimo-flash":               "mimo-v2.6-flash",
			"mimo-pro":                 "mimo-v2.6-pro",
			"mimo-ultraspeed":          "mimo-v2.6-pro-ultraspeed",
			"mimo-x-flash-preview":     "mimo-v2.6-flash",
			"mimo-x-pro-preview":       "mimo-v2.6-pro",
			"MiMo-X-Flash-Preview":     "mimo-v2.6-flash",
			"MiMo-X-Pro-Preview":       "mimo-v2.6-pro",
		},
		PrependSystem: false,
		Owner:         "xiaomi-mimo",
		LogPath:       "converter-mimo.log",
		DirectOnly:    true, // mimo-server-cn 国内可达，直连不读系统代理（切换网络/关代理软件也能用）
	}
}
