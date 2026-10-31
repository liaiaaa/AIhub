package main

// 线路元数据单一事实来源（2026-10-31 A2 第一步）。
//
// 参考 magpie 的做法（internal/provider/presets.go:162 的 presets []PresetDef）：
// 厂商/线路的「元数据」应该是数据行，不是散落在各处的字面量。本仓库此前的问题是
// 同一组事实写了三份：
//   - 端口：main.go / proxy_qoder.go / proxy_zcode.go / trae_proxy.go / proxy_mimo.go /
//     opencode_proxy.go 各定义常量，proxy_agg.go:71 又用字面量抄了一遍
//     （8787/8799/8797/8795/8796），platform.go 的 sweepPorts 再逐个列一遍；
//   - 线路名：hub.go 的 knownLineNames、main.go 的 /api/<line>/start 路由、
//     proxy_agg.go 的 aggLinePort、assets/console.html 的 SERVICES 数组各一套；
//   - 显示名/日志文件：控制台 JS 与各 proxy 构造 NewFileLogger 的地方各一套。
//
// 这一步只收敛「元数据」（名称/显示名/端口/日志文件/是否参与聚合/风控等级），
// 不改任何启停、转发、鉴权逻辑 —— 也就是行为零变化，纯去重。
//
// 下一步（A2 第二步）才会谈把 Start/Stop/streamToClient 收敛成统一实现。

// lineSpec 描述一条线路的静态元数据。
type lineSpec struct {
	Key     string // API 路径里的线路名：/api/<Key>/start
	Display string // 界面显示名（与 assets/console.html 的 SERVICES 保持一致）
	Port    int    // 本地监听端口
	LogFile string // 数据根下的日志文件名（converter-*.log）
	// Aggregated 表示该线路的模型会进 :8790 聚合口白名单。
	// mimo 不参与聚合（它是 SSO 通道，模型目录走另一套），所以为 false。
	Aggregated bool
	// Risk 是界面上要显示的风控等级：dom 上游 2026-10-23 收紧非官方客户端识别，
	// 国内反代到 agent 框架属明确高风险；其余线路为空。
	Risk string
}

// lineSpecs 是全部线路的唯一清单。顺序即 knownLineNames 的既有顺序，
// 不要随手调整（sanitizeLineNames 与前端展示都按这个顺序走）。
var lineSpecs = []lineSpec{
	{Key: "dom", Display: "国内 WorkBuddy", Port: DomPort, LogFile: "converter-cn.log", Aggregated: true, Risk: "高风险"},
	{Key: "intl", Display: "国际 WorkBuddy AI", Port: IntlPort, LogFile: "converter-intl.log", Aggregated: true},
	{Key: "qoder", Display: "Qoder CN（阿里）", Port: QoderPort, LogFile: "converter-qoder.log", Aggregated: true},
	{Key: "trae", Display: "Trae CN（字节）", Port: TraePort, LogFile: "converter-trae.log", Aggregated: true},
	{Key: "zcode", Display: "Zcode（Z.ai）", Port: ZcodePort, LogFile: "converter-zcode.log", Aggregated: true},
	{Key: "mimo", Display: "MiMo（小米）", Port: MimoPort, LogFile: "converter-mimo.log"},
	{Key: "oc", Display: "OpenCode 免费档", Port: OpencodePort, LogFile: "converter-opencode.log", Aggregated: true},
}

// lineKeys 返回全部线路名，顺序与 lineSpecs 一致。
func lineKeys() []string {
	out := make([]string, 0, len(lineSpecs))
	for _, s := range lineSpecs {
		out = append(out, s.Key)
	}
	return out
}

// aggregatedLinePorts 返回参与 :8790 聚合的线路 → 端口。
func aggregatedLinePorts() map[string]int {
	out := make(map[string]int, len(lineSpecs))
	for _, s := range lineSpecs {
		if s.Aggregated {
			out[s.Key] = s.Port
		}
	}
	return out
}

// allLinePorts 返回所有线路端口（启动清扫与端口冲突排查用）。
func allLinePorts() []int {
	out := make([]int, 0, len(lineSpecs))
	for _, s := range lineSpecs {
		out = append(out, s.Port)
	}
	return out
}
