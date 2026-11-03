package main

// 各线路的「出站身份」总表（2026-11-03，用户要求「其他代理线路也加上桌面端的身份伪装」）。
//
// 背景：各家官网的用量面板会把调用来源标出来（官方桌面端 → 「桌面端」，代理 → 空白或别的）。
// 前面 qoder 那次教训说明两件事：
//  1. 光对齐协议不够，**身份**（版本号、设备字段、请求号形态）要跟真客户端一致；
//  2. 「定义了但没接线」是真实故障模式（teleagent 的 X-TeleAI-Client-Type 就这样空了两个月）。
//
// 所以这里把每条线路「自称是谁」集中成一张表：来源、客户端名、版本、是否桌面端，
// 控制台直接读它展示（/api/lines 的 identity 字段），测试遍历 lineSpecs 保证不漏项。
//
// 注意：这张表**只描述事实**，不重复实现发送逻辑 —— 真正发头的地方仍是各家 proxy/
// identity 代码（dom/intl 在 officialChatHeaders、qoder 在 qoderIdentityPresets、
// teleagent 在 teleagentSignHeadersAt、trae/zcode 在各自 proxy）。表里写的是那些实现
// 实际发出去的值，改动实现时这张表要同步，测试会盯着「UA 里含客户端名」。

import "strings"

// lineIdentity 一条线路对外自称的身份。
type lineIdentity struct {
	Client    string // 客户端名，如 WorkBuddy / Qoder / Trae
	Version   string // 声明的版本（取不到就写「本机探测」）
	Desktop   bool   // 是否伪装成官方**桌面端**（false = 官方只有 CLI，或暂未对齐）
	UserAgent string // 实际发出的 UA（空 = 该线路不发 UA）
	Note      string // 依据 / 待办
}

// lineIdentities 按线路 key 索引。新增线路时这里必须补一行（测试会红）。
func lineIdentities() map[string]lineIdentity {
	return map[string]lineIdentity{
		"dom": {
			Client:    "WorkBuddy",
			Version:   officialWBVersion,
			Desktop:   true,
			UserAgent: officialDomUA(),
			Note:      "整套官方客户端指纹（UA/公共头/对话专项头/会话 id/链路头），见 docs/official-client.md",
		},
		"intl": {
			Client:    "WorkBuddy AI",
			Version:   officialIntlWBVersion,
			Desktop:   true,
			UserAgent: officialIntlUA(),
			Note:      "billing 域要求 UA 平台段含 \"WorkBuddy AI\"，故三段式",
		},
		"qoder": {
			Client:    "Qoder",
			Version:   qoderWorkerVersion(),
			Desktop:   true,
			UserAgent: "Qoder",
			Note:      "Cosy-ClientType 10 + worker 版本 + MachineToken/Type + 主机名 + UUID 请求号（2026-11-03 对齐 magpie 记录）",
		},
		"trae": {
			Client:    "Trae",
			Version:   traeIdeVersion,
			Desktop:   true,
			UserAgent: "Trae/" + traeIdeVersion,
			Note:      "X-App-Id / X-Ide-Version / X-Device-Type 等一整套 IDE 头",
		},
		"zcode": {
			Client:    "Zcode",
			Version:   "本机探测（zcodeDetectAppVersion）",
			Desktop:   true,
			UserAgent: "Zcode（由 zcode identity 组装）",
			Note:      "zcodeIdentity：appVersion/deviceMid/osVersion/platform 均按桌面端形态发",
		},
		"teleagent": {
			Client:    "TeleAgent",
			Version:   "teleagentAppVersion()",
			Desktop:   true,
			UserAgent: "TeleAgent/<appVersion>",
			Note:      "X-TeleAI-Client-Type: desktop（2026-11-03 修：该头此前全仓库零调用点，从没发出去）",
		},
		"oc": {
			Client:    "opencode",
			Version:   opencodeClientVer,
			Desktop:   false,
			UserAgent: "opencode/latest/" + opencodeClientVer + "/cli",
			Note:      "免费档官方只提供 CLI/TUI，没有桌面端；这里保留 CLI 身份，别硬装桌面端",
		},
		"mimo": {
			Client:    "MiClaw",
			Version:   "未探测（本机没装 MiMo 桌面端，取不到版本）",
			Desktop:   true,
			UserAgent: "MiClaw/1.0",
			Note:      "只有 UA，缺设备头；等有 MiMo 登录数据时再补齐（目前线路也用不了）",
		},
	}
}

// lineIdentityOf 取某线路的身份；ok=false 表示这张表没登记（新线路忘了补）。
func lineIdentityOf(line string) (lineIdentity, bool) {
	id, ok := lineIdentities()[line]
	return id, ok
}

// identitySummary 给控制台/接口用的精简视图。
func identitySummary(line string) map[string]any {
	id, ok := lineIdentityOf(line)
	if !ok {
		return nil
	}
	return map[string]any{
		"client":  id.Client,
		"version": id.Version,
		"desktop": id.Desktop,
		"ua":      id.UserAgent,
		"note":    id.Note,
	}
}

// lineIdentityUA 返回该线路应发的 UA（给没有自己 UA 逻辑的线路兜底用）。
func lineIdentityUA(line string) string {
	id, _ := lineIdentityOf(line)
	return strings.TrimSpace(id.UserAgent)
}
