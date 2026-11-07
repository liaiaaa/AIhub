package main

import (
	"strings"
	"testing"
)

// 探测面板必须真调同步接口，并把免费回执单独显示（用户 m05013/m05254）。
func TestConsoleProbeSyncWiring(t *testing.T) {
	src := consoleSource(t)
	for _, want := range []string{
		"/api/models/probe/sync",
		"function renderProbeFree(list)",
		"d.sync || {}",
		"mode:'supplement'",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("控制台探测接线缺少 %q", want)
		}
	}
}
