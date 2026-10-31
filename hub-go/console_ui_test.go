package main

// 控制台 UI 新增功能的静态回归测试（2026-10-31）。
//
// 风格与 consolejs_test.go / console_source_test.go 一致：控制台是一段内嵌 HTML，
// 没有前端构建步骤，所以靠「断言关键结构还在」来防止后续改页面时把这些功能改丢。
// 运行时的行为（真的点一下排序、真的下载 CSV）在预览实例上人工/截图验证过，
// 这里守的是「代码与 DOM 结构不许悄悄消失」。

import (
	"strings"
	"testing"
)

// TestConsoleLimitBoard 限流看板：线路卡片徽章 + 总览条目 + 数据来源。
func TestConsoleLimitBoard(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		`id="scool-`,        // 线路卡片冷却徽章
		`id="sfail-`,        // 最近失败行
		"function coolFor(", // 台账→线路归属
		"function renderLimits(",
		"/api/limits", // 数据来源
		"限流冷却 · ",     // 文案前缀
	} {
		if !strings.Contains(src, want) {
			t.Errorf("控制台缺少限流看板要素：%q", want)
		}
	}
}

// TestConsoleLogModal 日志弹层：DOM、打开/关闭、刷新、接口。
func TestConsoleLogModal(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		`id="log-mask"`,
		`id="log-body"`,
		"function openLog(",
		"function closeLog(",
		"function refreshLog(",
		"/api/logs/tail?file=",
		".modal-mask[hidden]", // 否则 display:flex 会盖掉 hidden
	} {
		if !strings.Contains(src, want) {
			t.Errorf("控制台缺少日志弹层要素：%q", want)
		}
	}
}

// TestConsoleUsageSortAndExport 用量页：三种排序 + CSV 导出四段。
func TestConsoleUsageSortAndExport(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		`id="usage-sort"`,
		`data-s="tok"`,
		`data-s="reqs"`,
		`data-s="fail"`,
		"function setUsageSort(",
		"function usageSortedModels(",
		"function exportUsageCSV(",
		`\ufeff`,                     // CSV 带 BOM，Excel 打开不乱码
		"csvLine(['日期'",              // 按天段
		"csvLine(['模型'",              // 按模型段
		"csvLine(['线路'",              // 按线路段
		"a.download = 'aihub-usage-", // 真的触发下载
	} {
		if !strings.Contains(src, want) {
			t.Errorf("用量页缺少要素：%q", want)
		}
	}
	// 榜单必须走排序函数，不能再直接 slice 原始数组
	if strings.Contains(src, "(usageData.models || []).slice(0, 10)") {
		t.Error("模型榜单仍在直接 slice 原始数组，排序功能会被绕过")
	}
}

// TestConsoleTodaySummary 总览「今日」摘要。
func TestConsoleTodaySummary(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		`id="hero-today"`,
		"function loadTodayUsage(",
		"function renderToday(",
		"/api/usage?days=1",
		"请求 ' + s.reqs", // 文案里带请求数
	} {
		if !strings.Contains(src, want) {
			t.Errorf("总览缺少今日摘要要素：%q", want)
		}
	}
}

// TestConsoleMaintenanceBlock 设置页维护区（只读）。
func TestConsoleMaintenanceBlock(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		`id="maint-logs"`,
		`id="maint-root"`,
		"function loadMaint(",
		"/api/logs/sizes",
		"运行维护",
		"这里只读不改", // 明确说明不做删除操作
	} {
		if !strings.Contains(src, want) {
			t.Errorf("设置页缺少维护区要素：%q", want)
		}
	}
}

// TestConsolePageAnchor 页面锚点：切页写 hash，启动按 hash 打开。
func TestConsolePageAnchor(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		"history.replaceState(null, '', '#' + name)",
		"const startPage = (location.hash || '').replace('#', '')",
		"switchPage(startPage)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("控制台缺少页面锚点逻辑：%q", want)
		}
	}
}
