package main

// 客户端画像（2026-11-05，用户要求「反代 workbuddy 就模拟 workbuddy 客户端，
// 反代 qoder 就模拟 qoder 客户端，以此类推」）。
//
// 一条线路「装成谁」由四个维度组成。前三个在这里集中定义，第四个引用既有实现：
//
//	1. TLS 指纹档（uTLS ClientHello）  → tlsfp.go 读 profile.TLS
//	2. HTTP/2 取舍（ALPN 是否带 h2）    → tlsfp.go 读 profile.HTTP2
//	3. 心跳节奏（间隔区间 / 静默时段）  → heartbeat.go 读 profile.HB*
//	4. 身份（UA / 版本 / 客户端名）     → line_identity.go（这里只引用，不复制）
//
// 为什么 h2 默认关（2026-11-05 社区调研结论）：
//
//   - misakano7545/traework2api-panel：对官方 Trae 客户端抓包是 **HTTP/1.1 +
//     Connection: keep-alive**；它把 uTLS 的 ALPN 收窄成只有 http/1.1，并用
//     `TLSNextProto = map[string]func(string,*tls.Conn) http.RoundTripper{}` 真禁 h2。
//   - linguo2625469/workbuddy2api-panel：注释给出实证 —— `ForceAttemptHTTP2=false`
//     不够，默认 TLS 经 ALPN 仍会协商 h2；要退回 1.1 必须给非 nil 的空 TLSNextProto。
//   - 反过来的代价：Go bundled http2 的 SETTINGS / priority 帧指纹与 Chromium 不同。
//     TLS 装成 Chrome、h2 帧却是 Go 的形状，比「Chrome 不带 h2」更显眼 ——
//     前者是「同一层自相矛盾」，后者只是「少一项能力」。
//
// 所以默认按抓包形态走 HTTP/1.1；确需 h2（例如某条线路实测只有 h2 才通）用
// AIHUB_HTTP2=1 或 AIHUB_HTTP2_<LINE>=1 开回来。

import (
	"os"
	"strings"
)

// clientWire 一条线路的「线上形态」：TLS 档 + h2 取舍 + 心跳节奏。
type clientWire struct {
	TLS       string // chrome|firefox|safari|edge|golang
	HTTP2     bool   // 是否走 h2（默认 false：见文件头）
	Heartbeat bool   // 是否有心跳探针
	HBMin     int    // 心跳最小间隔（秒）
	HBMax     int    // 心跳最大间隔（秒）
	QuietFrom int    // 静默开始（小时）
	QuietTo   int    // 静默结束（小时）
}

// clientWires 按线路 key。新增线路时这里必须补一行（client_profile_test 会遍历 lineSpecs 断言）。
//
// 心跳节奏的取值依据：官方客户端都是「几分钟拉一次目录/配置」，没有固定整点；
// 这里取 4~10 分钟区间，比对话请求稀疏一个数量级。qoder/zcode 的配置项更多，
// 用更长的 5~10 分钟；teleagent 的积分查询更轻，用 4~8 分钟。
var clientWires = map[string]clientWire{
	"dom":   {TLS: "chrome", Heartbeat: true, HBMin: 240, HBMax: 540, QuietFrom: 1, QuietTo: 7},
	"intl":  {TLS: "chrome", Heartbeat: true, HBMin: 240, HBMax: 540, QuietFrom: 1, QuietTo: 7},
	"qoder": {TLS: "chrome", Heartbeat: true, HBMin: 300, HBMax: 600, QuietFrom: 2, QuietTo: 7},
	// trae 没有心跳：唯一周期动作是 ExchangeToken，会轮换 refreshToken 和真实请求抢。
	"trae":      {TLS: "chrome", Heartbeat: false, HBMin: 300, HBMax: 720, QuietFrom: 1, QuietTo: 7},
	"zcode":     {TLS: "chrome", Heartbeat: true, HBMin: 300, HBMax: 600, QuietFrom: 2, QuietTo: 7},
	"teleagent": {TLS: "chrome", Heartbeat: true, HBMin: 240, HBMax: 480, QuietFrom: 2, QuietTo: 7},
	// opencode 官方只有 CLI（Node/undici），TLS 保持 Go 形状，别硬装浏览器。
	"oc": {TLS: "golang", Heartbeat: false},
	// 线路还没打通，先按桌面端登记，等能用了再补探针。
	"mimo": {TLS: "chrome", Heartbeat: false},
	// 果办走华为官方 API 网关（Basic 认证 + 服务端计费），没有客户端指纹校验，
	// 也没有需要模拟的周期动作（额度/签到由用户手动触发，见 main.go:160 的口径）。
	"huawei": {TLS: "golang", Heartbeat: false},
}

// clientProfile 线路画像的完整视图（给控制台/接口看，也给内部判断用）。
type clientProfile struct {
	Line      string `json:"line"`
	Display   string `json:"display"`
	Client    string `json:"client"`
	Desktop   bool   `json:"desktop"`
	TLS       string `json:"tls"`
	HTTP2     bool   `json:"http2"`
	Heartbeat bool   `json:"heartbeat"`
	HBMin     int    `json:"hb_min"`
	HBMax     int    `json:"hb_max"`
	QuietFrom int    `json:"quiet_from"`
	QuietTo   int    `json:"quiet_to"`
	Note      string `json:"note,omitempty"`
}

// clientProfileFor 取一条线路的画像。未知线路给一份保守默认（chrome + h1 + 无心跳）。
func clientProfileFor(line string) clientProfile {
	w, ok := clientWires[line]
	if !ok {
		w = clientWire{TLS: "chrome"}
	}
	p := clientProfile{
		Line:      line,
		TLS:       w.TLS,
		HTTP2:     http2ForLine(line, w.HTTP2),
		Heartbeat: w.Heartbeat,
		HBMin:     w.HBMin,
		HBMax:     w.HBMax,
		QuietFrom: w.QuietFrom,
		QuietTo:   w.QuietTo,
	}
	if spec, ok := lineSpecByKey(line); ok {
		p.Display = spec.Display
	}
	if id, ok := lineIdentityOf(line); ok {
		p.Client = id.Client
		p.Desktop = id.Desktop
		p.Note = id.Note
	}
	return p
}

// lineSpecByKey 找线路元数据（lines.go 的 lineSpecs）。
func lineSpecByKey(key string) (lineSpec, bool) {
	for _, s := range lineSpecs {
		if s.Key == key {
			return s, true
		}
	}
	return lineSpec{}, false
}

// http2ForLine 解析 h2 取舍：按线路 env > 全局 env > 画像默认。
func http2ForLine(line string, def bool) bool {
	if line != "" {
		name := "AIHUB_HTTP2_" + strings.ToUpper(strings.ReplaceAll(line, "-", "_"))
		if v, ok := boolEnv(name); ok {
			return v
		}
	}
	if v, ok := boolEnv("AIHUB_HTTP2"); ok {
		return v
	}
	return def
}

// boolEnv 读一个布尔环境变量；ok=false 表示没设或写的是无法识别的值。
func boolEnv(name string) (bool, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "on", "true", "yes", "enable", "enabled", "h2", "http2":
		return true, true
	case "0", "off", "false", "no", "disable", "disabled", "h1", "http1", "http/1.1":
		return false, true
	}
	return false, false
}

// clientProfilesSummary 给 /api/status 与 /api/tls-fingerprint 的只读视图。
func clientProfilesSummary() []clientProfile {
	out := make([]clientProfile, 0, len(lineSpecs))
	for _, s := range lineSpecs {
		out = append(out, clientProfileFor(s.Key))
	}
	return out
}

// clientProfileLines 返回画像表里登记过的线路（测试用）。
func clientProfileLines() []string {
	out := make([]string, 0, len(clientWires))
	for k := range clientWires {
		out = append(out, k)
	}
	return out
}
