package main

// Zcode 桌面端身份头（2026-09-27 自 TriDefender/zcode-api 的 identity.ts 逆向移植，
// 对齐 ZCode 桌面端 3.12.3/3.14.3 的两套 header 组）：
//
//   g6n = LLM 补全面（chat/completions 等推理请求）：HTTP-Referer、User-Agent
//         ZCode/<ver>、[X-ZCode-App-Version]、X-Title "Z Code@<title>"、
//         X-Release-Channel、X-Client-Language、X-Client-Timezone、
//         X-ZCode-Agent "glm"、[X-Platform]、X-Os-Category、[X-Os-Version]。
//         绝不带 X-Device-Mid。
//   TV  = 控制面（agent/configs、client/configs、billing/balance）：与 g6n 的差别
//         是去掉 X-ZCode-Agent、加上 X-Device-Mid（~/.zcode/v2/telemetry-state.json）。
//
// 官方客户端的 LLM 请求就落在 z-ai openai:chat 通道（client/configs 下发
// baseUrl=https://api.z.ai/api/coding/paas/v4，与本代理 Endpoint 相同），
// 补齐这组头即可与官方流量在指纹层对齐。所有值过桌面端同款可打印 ASCII 门；
// 条件头（版本/平台/OS 版本/设备号）取不到时整体省略，不发明假值。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// zcodeIdentity 桌面端身份值集（与 zcode-api ProxyIdentity 对齐）。
type zcodeIdentity struct {
	appVersion     string
	sourceTitle    string
	refererOrigin  string
	deviceMid      string
	platform       string // Node os.platform() 形态：win32/darwin/linux
	arch           string // Node process.arch() 形态：x64/arm64
	osVersion      string // Node os.release() 形态：windows "10.0.<build>"
	releaseChannel string
	clientLanguage string
	clientTimezone string
}

// zcodeLoadIdentity 汇集本机身份值。文件缺失/字段缺失时留空，
// 由各 header 构造器决定省略还是用官方同款 "unknown" 兜底。
func zcodeLoadIdentity() *zcodeIdentity {
	id := &zcodeIdentity{
		sourceTitle:    zcodePrintable(envOr("ZCODE_SOURCE_TITLE", "cli")),
		refererOrigin:  zcodePrintable(envOr("ZCODE_REFERER_ORIGIN", "https://zcode.z.ai")),
		releaseChannel: zcodePrintable(envOr("ZCODE_RELEASE_CHANNEL", "production")),
		clientLanguage: zcodePrintable(envOr("ZCODE_CLIENT_LANGUAGE", "zh-CN")),
		clientTimezone: zcodePrintable(zcodeClientTimezone()),
		platform:       zcodeNodePlatform(),
		arch:           zcodeNodeArch(),
	}
	id.appVersion = zcodePrintable(envOr("ZCODE_APP_VERSION", ""))
	if id.appVersion == "" {
		id.appVersion = zcodePrintable(zcodeDetectAppVersion())
	}
	id.deviceMid = zcodePrintable(zcodeDetectDeviceMid())
	id.osVersion = zcodePrintable(zcodeDetectOsVersion())
	return id
}

// platformParam 配置接口的 platform 查询参数（官方 win32-x64 形态）。
func (id *zcodeIdentity) platformParam() string {
	return id.platform + "-" + id.arch
}

// osCategory X-Os-Category 取值（桌面端把原始平台映射为三值）。
func (id *zcodeIdentity) osCategory() string {
	switch id.platform {
	case "win32":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

// llmHeaders g6n 组：LLM 补全请求身份头（无 X-Device-Mid）。
func (id *zcodeIdentity) llmHeaders() map[string]string {
	if id == nil {
		return nil
	}
	ua := "ZCode/unknown"
	if id.appVersion != "" {
		ua = "ZCode/" + id.appVersion
	}
	h := map[string]string{
		"HTTP-Referer":      id.refererOrigin,
		"User-Agent":        ua,
		"X-Title":           "Z Code@" + id.sourceTitle,
		"X-Release-Channel": id.releaseChannel,
		"X-Client-Language": orStr(id.clientLanguage, "unknown"),
		"X-Client-Timezone": orStr(id.clientTimezone, "unknown"),
		"X-ZCode-Agent":     "glm",
	}
	if id.appVersion != "" {
		h["X-ZCode-App-Version"] = id.appVersion
	}
	if id.platform != "" && id.arch != "" {
		h["X-Platform"] = id.platform + "-" + id.arch
	}
	if id.osVersion != "" {
		h["X-Os-Version"] = id.osVersion
	}
	if cat := id.osCategory(); cat != "" {
		h["X-Os-Category"] = cat
	}
	return h
}

// contextHeaders TV 组：控制面请求身份头（无 X-ZCode-Agent，带 X-Device-Mid）。
func (id *zcodeIdentity) contextHeaders() map[string]string {
	if id == nil {
		return nil
	}
	ua := "ZCode/unknown"
	if id.appVersion != "" {
		ua = "ZCode/" + id.appVersion
	}
	h := map[string]string{
		"User-Agent":        ua,
		"HTTP-Referer":      id.refererOrigin,
		"X-Title":           "Z Code@" + id.sourceTitle,
		"X-Client-Language": orStr(id.clientLanguage, "unknown"),
		"X-Client-Timezone": orStr(id.clientTimezone, "unknown"),
	}
	if id.appVersion != "" {
		h["X-ZCode-App-Version"] = id.appVersion
	}
	if id.platform != "" && id.arch != "" {
		h["X-Platform"] = id.platform + "-" + id.arch
	}
	if id.releaseChannel != "" {
		h["X-Release-Channel"] = id.releaseChannel
	}
	if id.platform != "" {
		if cat := id.osCategory(); cat != "" {
			h["X-Os-Category"] = cat
		}
	}
	if id.osVersion != "" {
		h["X-Os-Version"] = id.osVersion
	}
	if id.deviceMid != "" {
		h["X-Device-Mid"] = id.deviceMid
	}
	return h
}

// setIdentityHeaders 把身份头写进请求。直接对 map 赋值以保留 "HTTP-Referer"
// 的官方大小写（Header.Set 会规范化成 "Http-Referer"，本机走 HTTP/1.1 可保真）。
func setIdentityHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if v == "" {
			continue
		}
		req.Header[k] = []string{v}
	}
}

// zcodePrintable 桌面端可打印 ASCII 门（fio/tq）：trim 后非空且全为 0x20-0x7e。
func zcodePrintable(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return v
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// zcodeNodeArch 对齐 Node process.arch() 的取值。
func zcodeNodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return runtime.GOARCH
	}
}

var zcodeVersionDirRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// zcodeDetectAppVersion 从桌面端运行时目录推版本：
// ~/.zcode/v2/runtime/provider/<平台>/<x.y.z>/，取最高语义化版本
// （0.0.0-dev 之类的占位目录被正则整体排除）。官方 appVersion 就是这个版本。
func zcodeDetectAppVersion() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	base := filepath.Join(home, ".zcode", "v2", "runtime", "provider")
	platDirs, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	best := ""
	var bestParts [3]int
	for _, pd := range platDirs {
		if !pd.IsDir() {
			continue
		}
		vers, err := os.ReadDir(filepath.Join(base, pd.Name()))
		if err != nil {
			continue
		}
		for _, vd := range vers {
			m := zcodeVersionDirRe.FindStringSubmatch(vd.Name())
			if m == nil {
				continue
			}
			var parts [3]int
			for i := 0; i < 3; i++ {
				parts[i], _ = strconv.Atoi(m[i+1])
			}
			if best == "" || parts[0] > bestParts[0] ||
				(parts[0] == bestParts[0] && parts[1] > bestParts[1]) ||
				(parts[0] == bestParts[0] && parts[1] == bestParts[1] && parts[2] > bestParts[2]) {
				best, bestParts = vd.Name(), parts
			}
		}
	}
	return best
}

// zcodeDetectDeviceMid 读桌面端遥测状态里的设备号（官方 X-Device-Mid 来源，
// 一次性生成永久复用）。
func zcodeDetectDeviceMid() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".zcode", "v2", "telemetry-state.json"))
	if err != nil {
		return ""
	}
	var st struct {
		DeviceMid string `json:"deviceMid"`
	}
	if json.Unmarshal(raw, &st) != nil {
		return ""
	}
	return strings.TrimSpace(st.DeviceMid)
}

// winTimeZoneNames Windows 时区名 → IANA（X-Client-Timezone 用，与桌面端
// Intl timezone 对齐）。覆盖常用值，查不到回退本部署实际时区 Asia/Shanghai。
var winTimeZoneNames = map[string]string{
	"China Standard Time":          "Asia/Shanghai",
	"UTC":                          "UTC",
	"GMT Standard Time":            "Europe/London",
	"W. Europe Standard Time":      "Europe/Berlin",
	"Central Europe Standard Time": "Europe/Prague",
	"Romance Standard Time":        "Europe/Paris",
	"E. Europe Standard Time":      "Europe/Bucharest",
	"GTB Standard Time":            "Europe/Athens",
	"Tokyo Standard Time":          "Asia/Tokyo",
	"Korea Standard Time":          "Asia/Seoul",
	"Taipei Standard Time":         "Asia/Taipei",
	"Hong Kong Standard Time":      "Asia/Hong_Kong",
	"Singapore Standard Time":      "Asia/Singapore",
	"India Standard Time":          "Asia/Kolkata",
	"Arabian Standard Time":        "Asia/Dubai",
	"US Eastern Standard Time":     "America/New_York",
	"Eastern Standard Time":        "America/New_York",
	"Central Standard Time":        "America/Chicago",
	"Mountain Standard Time":       "America/Denver",
	"Pacific Standard Time":        "America/Los_Angeles",
}

// zcodeClientTimezone X-Client-Timezone 取值：环境变量 > 注册表 Windows 时区名
// 映射 > Asia/Shanghai（与桌面端同机部署时的实际 Intl timezone 一致）。
func zcodeClientTimezone() string {
	if tz := os.Getenv("ZCODE_CLIENT_TIMEZONE"); strings.TrimSpace(tz) != "" {
		return strings.TrimSpace(tz)
	}
	if runtime.GOOS == "windows" {
		name := regQueryValue(`HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation`, "TimeZoneKeyName")
		if iana, ok := winTimeZoneNames[strings.TrimSpace(name)]; ok {
			return iana
		}
	}
	return "Asia/Shanghai"
}

// zcodeDetectOsVersion 对齐 Node os.release()：windows 走注册表构建号
// （"10.0.<build>"，与官方客户端一致；os.KernelVersion 标准库不存在）。
// 其他平台取不到就返回空，header 整体省略。
func zcodeDetectOsVersion() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	build := regQueryValue(`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "CurrentBuildNumber")
	build = strings.TrimSpace(build)
	if build == "" {
		return ""
	}
	// Node os.release() 在 Win11 上同样返回 10.0.<build>，主版本恒为 10
	return "10.0." + build
}
