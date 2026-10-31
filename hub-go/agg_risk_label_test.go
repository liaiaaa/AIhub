package main

// dom 高风险标注（2026-10-25 用户选 A：保留 dom 可选，但名字上标红）。
//
// 只改显示名，不改模型 id 与路由——用户仍然可以手动选 dom，只是选之前
// 一眼能看到风险。前缀必须幂等：Display 可能来自旧快照，也可能刚由
// aggDisplayName 生成，两处都会过 aggRiskTagged。

import (
	"strings"
	"testing"
)

func TestAggDisplayNameTagsDomOnly(t *testing.T) {
	dom := aggDisplayName("dom", "glm-5.3-flash")
	if !strings.HasPrefix(dom, aggRiskTag) {
		t.Errorf("dom 显示名应带高风险前缀，实际 %q", dom)
	}
	if !strings.Contains(dom, "（国内）") {
		t.Errorf("dom 显示名丢了线路归属：%q", dom)
	}
	for _, line := range []string{"intl", "qoder", "trae", "zcode", "oc"} {
		got := aggDisplayName(line, "glm-5.3-flash")
		if strings.Contains(got, aggRiskTag) {
			t.Errorf("%s 不该带高风险前缀：%q", line, got)
		}
	}
}

func TestAggRiskTaggedIsIdempotent(t *testing.T) {
	once := aggRiskTagged("dom", "GLM 5.3 Flash（国内）")
	twice := aggRiskTagged("dom", once)
	if once != twice {
		t.Errorf("重复标注：%q → %q", once, twice)
	}
	if got := aggRiskTagged("intl", "GLM 5.3 Flash（国际）"); strings.Contains(got, aggRiskTag) {
		t.Errorf("intl 不该被标：%q", got)
	}
	if got := aggRiskTagged("dom", ""); got != "" {
		t.Errorf("空名字不该变成只有前缀：%q", got)
	}
}
