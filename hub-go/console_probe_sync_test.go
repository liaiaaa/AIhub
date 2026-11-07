package main

import (
	"strings"
	"testing"
)

// 探测面板必须真调同步接口、把免费回执单独显示，且每条线路可折叠
// （用户 m05603：「更新的是数据库不是我的目录模型」「每个线路加个可折叠按钮」）。
func TestConsoleProbeSyncWiring(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		"/api/models/probe/sync",
		"function renderProbeFree(list)",
		"d.sync || {}",
		"不改你的目录模型",
		"<details class=\"probe-line\"",
		"probeOpen",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("控制台探测接线缺少 %q", want)
		}
	}
	if strings.Contains(src, "mode:'supplement'") {
		t.Fatal("同步已不碰目录模型，「只补充不删」按钮应移除")
	}
}
