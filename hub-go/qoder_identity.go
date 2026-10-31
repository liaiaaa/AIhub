package main

// qoder 出站身份：把「我们自称是谁」集中到一处，避免头与体各说各话。
//
// 2026-10-27 审计背景（改之前的状态）：同一条请求里混了三个身份——
//   - COSY 签名 + Cosy-Version 1.0.0        ← 桌面端 HTe() 的形态
//   - Cosy-ClientType 0 / X-IDE-Platform Qoder / X-Version 0.3.4
//     ← 2026-09 那份旧 CLI 抓包（qoder-sandbox/QODER-HTTP-交接文档.md:127-141）
//   - session_type qodercli / business.product cli / business.version 1.0.60
//     ← 又一套 CLI 形态
// 三个来源拼在一起，任何字段一致性交叉校验都能命中「不是官方客户端」。
//
// 现在两种预设二选一，头与体**一起**换：
//
//	app —— 桌面端 Qoder CN。官方常量
//	       yc = {clientType:10, businessProduct:"app", sessionType:"app"}
//	       UA "Qoder"、Accept "text/event-stream, application/json"、
//	       Accept-Encoding "identity"。
//	       hub 的 COSY 签名本来就是照桌面端 HTe()/QPt() 实现的，选它最自洽。
//	       注：桌面端 bundle 里另有一处 Accept "application/json"，但实测
//	       agent_chat_generation 用它会直接 500（2026-10-27 二分确认），
//	       所以这里取本端点可用的 SSE 形态。
//
//	cli —— 新版 CN CLI runtime（Cosy-ClientType 5、Cosy-MachineOS、
//	       Accept-Language en-US、session_type qoderclicn）。
//	       注意：CLI runtime 里没有任何 COSY 字面量（Bearer COSY / Cosy-Key /
//	       Cosy-Date / Cosy-User 均 0 处），它换了鉴权方式；hub 没有那条链路，
//	       所以这条预设只是「头对齐、鉴权仍是 COSY」，自洽度不如 app。
//
// 默认 app；`QODER_IDENTITY=cli` 或数据根 `qoder_identity.txt` 可切换，
// 每请求现读，改完不用重启。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// qoder 官方客户端版本兜底值（读不到本机安装时使用）。
//
// app 取自本机 ~/.qoder-cn/.qoder-app-status.json 的实测值（2026-10-27）；
// cli 取自 CN CLI runtime 包内的 pie/Oke 常量（qoder-worker-runtime.obf.mjs）。
const (
	qoderAppVersionFallback = "0.4.8"
	qoderCLIVersionFallback = "1.1.57"
)

// qoderIdentity 是一整套「客户端自称」的取值。头与体都从这里取，
// 保证同一条请求里不出现两个版本的自我描述。
type qoderIdentity struct {
	Name string // 预设名，只用于日志

	ClientType  string // Cosy-ClientType
	IDEPlatform string // X-IDE-Platform
	Product     string // Cosy-Business-Product / business.product
	SessionType string // 信封 session_type
	UserAgent   string // User-Agent（空=不发，交给 Go 默认）
	Accept      string
	AcceptEnc   string // Accept-Encoding（空=不发）
	AcceptLang  string // Accept-Language（空=不发）
	CosyVersion string // Cosy-Version（桌面端缺省就是 1.0.0）
	Version     string // 客户端版本：X-Version / payload.ideVersion / business.version

	SendMachineOS bool // 是否发 Cosy-MachineOS / X-Machine-OS
	SendHostname  bool // 是否发 Cosy-MachineHostname（官方仅 infer-sse 发）
}

// qoderIdentityPresets 两套预设。Version 字段在构造时按本机安装探测后填入。
var qoderIdentityPresets = map[string]qoderIdentity{
	"app": {
		Name:          "app",
		ClientType:    "10",
		IDEPlatform:   "app",
		Product:       "app",
		SessionType:   "app",
		UserAgent:     "Qoder",
		Accept:        "text/event-stream, application/json",
		AcceptEnc:     "identity",
		CosyVersion:   "1.0.0",
		SendMachineOS: true,
		SendHostname:  false,
	},
	"cli": {
		Name:          "cli",
		ClientType:    "5",
		IDEPlatform:   "qoder_cli",
		Product:       "cli",
		SessionType:   "qoderclicn",
		UserAgent:     "Qoder CLI CN", // BRAND_CLI_NAME（CN 构建）
		Accept:        "text/event-stream, application/json",
		AcceptLang:    "en-US",
		CosyVersion:   qoderCLIVersionFallback,
		SendMachineOS: true,
		SendHostname:  true,
	},
}

// qoderIdentityEnv / qoderIdentityFile 是切换开关：环境变量优先，
// 其次数据根文件（桌面快捷方式启动带不了环境变量）。
const (
	qoderIdentityEnv  = "QODER_IDENTITY"
	qoderIdentityFile = "qoder_identity.txt"
)

var (
	qoderVerMu      sync.Mutex
	qoderAppVersion string
	qoderCLIVersion string
	qoderVerAt      time.Time
)

// qoderVersionTTL 版本探测结果的缓存时长。
//
// 2026-10-27 修：原实现用 sync.Once 缓存一辈子，而桌面端**会自己升级**。
// 实测事故：进程 18:30 启动、18:31 首次探测拿到 0.4.8 并永久缓存；
// 桌面端 18:32 升级到 0.4.11，hub 之后每次请求仍报 0.4.8 —— 版本号永远
// 落后一个升级周期，正是「出站身份与真实客户端不一致」这类指纹的来源。
//
// 现在改成带 TTL 的缓存：10 分钟重探一次（一次 stat+read 一个小文件，
// 摊到每个请求上可忽略），桌面端升级后最多 10 分钟就跟上。
const qoderVersionTTL = 10 * time.Minute

// qoderVersions 探测本机安装版本，探测不到用兜底常量。
// 结果按 qoderVersionTTL 缓存，过期重探。
func qoderVersions() (app, cli string) {
	qoderVerMu.Lock()
	defer qoderVerMu.Unlock()
	if qoderAppVersion == "" || time.Since(qoderVerAt) > qoderVersionTTL {
		home, err := os.UserHomeDir()
		if err != nil {
			home = ""
		}
		qoderAppVersion = qoderProbeAppVersion(home)
		qoderCLIVersion = qoderCLIVersionFallback
		qoderVerAt = time.Now()
	}
	return qoderAppVersion, qoderCLIVersion
}

// qoderProbeAppVersion 从桌面端状态文件读版本号；读不到/字段不符回落常量。
func qoderProbeAppVersion(home string) string {
	if home == "" {
		return qoderAppVersionFallback
	}
	// 桌面端状态文件里带着真实版本号（{logged_in, version, product, ...}）
	b, err := os.ReadFile(filepath.Join(home, ".qoder-cn", ".qoder-app-status.json"))
	if err != nil {
		return qoderAppVersionFallback
	}
	var st struct {
		Version string `json:"version"`
		Product string `json:"product"`
	}
	if json.Unmarshal(b, &st) != nil {
		return qoderAppVersionFallback
	}
	if v := strings.TrimSpace(st.Version); v != "" && st.Product == "qodercn" {
		return v
	}
	return qoderAppVersionFallback
}

// qoderIdentityName 解析当前该用哪套身份：环境变量 > 数据根文件 > 默认 app。
// 非法值忽略（回落默认），避免一个手滑把线路打挂。
func qoderIdentityName() string {
	if v := strings.TrimSpace(os.Getenv(qoderIdentityEnv)); v != "" {
		if _, ok := qoderIdentityPresets[strings.ToLower(v)]; ok {
			return strings.ToLower(v)
		}
	}
	if b, err := os.ReadFile(dataPath(qoderIdentityFile)); err == nil {
		v := strings.ToLower(strings.TrimSpace(string(b)))
		if _, ok := qoderIdentityPresets[v]; ok {
			return v
		}
	}
	return "app"
}

// currentQoderIdentity 取当前生效的身份（版本号已按本机安装填好）。
func currentQoderIdentity() qoderIdentity {
	name := qoderIdentityName()
	id := qoderIdentityPresets[name]
	app, cli := qoderVersions()
	if name == "cli" {
		id.Version = cli
	} else {
		id.Version = app
	}
	return id
}

// qoderMachineOS 官方 Cosy-MachineOS 的形态：`<arch>_<platform>`，
// 例如 x86_64_linux（qoder-worker-runtime 里 xie 的拼法）。
func qoderMachineOS() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	case "386":
		arch = "x86"
	}
	return arch + "_" + runtime.GOOS
}

// qoderSanitizeEnabled 是否清洗 system prompt 里的框架身份句与 ACP 标签。
// 默认开启；QODER_SANITIZE=0/off/false 关闭（排查用）。
func qoderSanitizeEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QODER_SANITIZE"))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}
