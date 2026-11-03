package main

// 2026-11-03：控制台「管理线路」入口与显隐过滤的静态断言。

import "strings"

import "testing"

func TestConsoleLinesAdmin(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		`id="lines-mask"`,       // 管理弹层
		`id="lines-admin-body"`, // 弹层内容
		"openLinesAdmin()",      // 线路页入口按钮
		"setLineVisible(",       // 隐藏/显示动作
		"function lineVisible(", // 渲染过滤函数
		"loadLines(false)",      // 启动时拉一次
	} {
		if !strings.Contains(src, want) {
			t.Errorf("控制台缺少 %q", want)
		}
	}
	// 两处磁贴渲染都必须先过滤隐藏线路，否则隐藏了还在页面上
	if strings.Count(src, "SERVICES.filter(function (s) { return lineVisible(s.key); })") != 2 {
		t.Errorf("两处 SERVICES 渲染都要过滤隐藏线路，实际匹配 %d 处",
			strings.Count(src, "SERVICES.filter(function (s) { return lineVisible(s.key); })"))
	}
}
