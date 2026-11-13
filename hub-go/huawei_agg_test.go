package main

// 果办线路「能被聚合口与目录模型页识别」的守卫测试（2026-11-13）。
//
// 背景（用户报障）：「aihub 的模型-目录模型中找不到果办 OfficeAce 线路，
// 导致无法添加模型到聚合线路」。根因是新增线路时只登记了 lineSpecs，
// 漏掉了「参与聚合」这一整条链上的 6 个登记点：
//   ① lines.go lineSpecs.Aggregated  → 聚合端口表 aggregatedLinePorts
//   ② proxy_agg.go aggSelectableLine → 目录下拉/加入校验的白名单
//   ③ proxy_agg.go aggLineCN         → 用量表/提示里的中文线路名
//   ④ model_probe.go modelProbeLines → 「探测新模型」是否覆盖该线路
//   ⑤ model_probe.go probeLine 分支  → 探测该线路时的模型清单来源
//   ⑥ assets/console.html agg-line   → 目录模型页的线路下拉选项
//
// 这条测试把①~⑤ 钉死；⑥ 由 TestConsoleAggLineOptionsCoverLineSpecs 覆盖。

import (
	"regexp"
	"strings"
	"testing"
)

// TestHuaweiLineFullyRegistered 果办线路在聚合链上每个登记点都存在。
func TestHuaweiLineFullyRegistered(t *testing.T) {
	const key = "huawei"

	// ① 参与聚合 + 端口
	spec, ok := lineSpecByKey(key)
	if !ok {
		t.Fatalf("lineSpecs 里没有 %s", key)
	}
	if !spec.Aggregated {
		t.Error("huawei 的 Aggregated=false：它不会进聚合口白名单，目录模型页也选不到")
	}
	if p, ok := aggregatedLinePorts()[key]; !ok || p != spec.Port {
		t.Errorf("aggregatedLinePorts 缺 %s 或端口不符：got %d want %d", key, p, spec.Port)
	}

	// ② 目录可加入白名单
	if !aggSelectableLine(key) {
		t.Error("aggSelectableLine(huawei)=false：/api/agg/line_models 与 /api/agg/add 都会拒绝它")
	}

	// ③ 中文线路名（用量表/错误提示用）
	if aggLineCN[key] == "" {
		t.Error("aggLineCN 缺 huawei：用量统计与「线路未启动」提示会显示空名字")
	}

	// ④ 「探测新模型」覆盖
	found := false
	for _, l := range modelProbeLines {
		if l == key {
			found = true
			break
		}
	}
	if !found {
		t.Error("modelProbeLines 缺 huawei：模型探测页看不到这条线路")
	}
}

// TestHuaweiProbeLineReturnsModels 探测果办线路必须给出模型清单与倍率。
func TestHuaweiProbeLineReturnsModels(t *testing.T) {
	h := NewHub()
	pl := probeLine(h, "huawei")
	if !pl.Up {
		t.Error("果办探测结果 Up=false：应为「线路可用」（静态模型表）")
	}
	if len(pl.Models) == 0 {
		t.Fatal("果办探测没有模型：目录下拉会是空的")
	}
	if len(pl.Models) != len(hwStaticModels) {
		t.Errorf("果办模型数 = %d，期望静态表 %d 个", len(pl.Models), len(hwStaticModels))
	}
	// 每个模型都要有倍率文案（果办按积分计费，不能显示「免费」）
	missing := []string{}
	for _, m := range pl.Models {
		if m.Ratio == "" {
			missing = append(missing, m.ID)
		}
		if m.Free {
			t.Errorf("果办模型 %s 被标成免费——它是积分计费的", m.ID)
		}
	}
	if len(missing) > 0 {
		t.Errorf("这些果办模型没有倍率（model_ratios.go 的 huawei 表漏了）：%v", missing)
	}
	// 上下文元数据：全 1M（插件 FALLBACK_MODELS 的声明值）
	for _, m := range pl.Models {
		if m.Meta == nil {
			t.Errorf("果办模型 %s 没有元数据（上下文窗口）", m.ID)
			continue
		}
		// metaJSON 的输出键是 "ctx"（不是 context_window——那个是 modelMeta 的内部字段名）
		if ctx, _ := m.Meta["ctx"].(int); ctx != 1048576 {
			t.Errorf("果办模型 %s 上下文 = %v，期望 1048576", m.ID, m.Meta["ctx"])
		}
	}
}

// TestHuaweiSlugResolvesToLine 目录里加入的 huawei.<model> 必须能被网关解析。
func TestHuaweiSlugResolvesToLine(t *testing.T) {
	// <线路>.<模型> 动态形式
	am, ok := resolveAggModel("huawei.deepseek-v4.1-flash")
	if !ok {
		t.Fatal("huawei.deepseek-v4.1-flash 解析失败：聚合口会回「未知模型」")
	}
	if am.Line != "huawei" || am.Backend != "deepseek-v4.1-flash" {
		t.Errorf("解析结果 line=%q backend=%q，期望 huawei / deepseek-v4.1-flash", am.Line, am.Backend)
	}

	// ccs- 形态（Claude Desktop 的写法）
	am2, ok2 := resolveAggModel("ccs-huawei/deepseek-v4.1-flash")
	if !ok2 {
		t.Fatal("ccs-huawei/… 解析失败：Claude Desktop 加不了果办模型")
	}
	if am2.Line != "huawei" || am2.Backend != "deepseek-v4.1-flash" {
		t.Errorf("ccs 解析结果 line=%q backend=%q", am2.Line, am2.Backend)
	}
}

// TestConsoleAggLineOptionsCoverLineSpecs 目录模型页的线路下拉必须覆盖全部可聚合线路。
//
// 这是用户报障的直接表现面：后端登记了、下拉里没有 → 界面上「找不到这条线路」。
func TestConsoleAggLineOptionsCoverLineSpecs(t *testing.T) {
	src := consoleSource(t)
	// 取 <select id="agg-line"> 到 </select> 之间的 option value
	re := regexp.MustCompile(`(?s)<select id="agg-line".*?</select>`)
	block := re.FindString(src)
	if block == "" {
		t.Fatal("控制台里没找到 agg-line 下拉")
	}
	optRe := regexp.MustCompile(`<option value="([a-z]+)"`)
	got := map[string]bool{}
	for _, m := range optRe.FindAllStringSubmatch(block, -1) {
		got[m[1]] = true
	}
	for _, s := range lineSpecs {
		if !s.Aggregated {
			continue // 不参与聚合的线路不该出现在下拉里（mimo）
		}
		if !got[s.Key] {
			t.Errorf("目录模型页的线路下拉缺 %q（lines.go 里它 Aggregated=true）——用户会「找不到这条线路」", s.Key)
		}
	}
	// 反向：下拉里不能有后端不认的线路
	for k := range got {
		if !aggSelectableLine(k) {
			t.Errorf("下拉里有 %q，但 aggSelectableLine 不认它（选中后接口会 400）", k)
		}
	}
}

// TestHuaweiSlugCNDisplay slug 的中文前缀映射覆盖果办（目录表格显示用）。
func TestHuaweiSlugCNDisplay(t *testing.T) {
	src := consoleSource(t)
	if !strings.Contains(src, "'huawei.': '果办·'") {
		t.Error("控制台 SLUG_CN 缺 'huawei.' 映射：目录表格会显示原始 slug 而不是「果办·xxx」")
	}
	if !strings.Contains(src, "'ccs-huawei/': '果办·'") {
		t.Error("控制台 SLUG_CN 缺 'ccs-huawei/' 映射")
	}
}
