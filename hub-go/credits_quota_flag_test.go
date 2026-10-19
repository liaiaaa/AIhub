package main

// 前端 SERVICES 的 quota 标记必须与后端 creditKeys 一致。
//
// 不一致有两种坏法：后端能查而前端没标 → 那行额度永远不显示（用户查不到）；
// 前端标了而后端不认 → 点「刷新额度」只会拿到「未知平台」（这次就是 OpenCode
// 免费档的情形：它压根没有账号额度，匿名档 Bearer public）。
// 这里用源码比对，避免再靠人肉同步两张清单。

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestServiceQuotaFlagsMatchCreditKeys(t *testing.T) {
	src, err := os.ReadFile("webui.go")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const SERVICES = \[.*?\];`).FindString(string(src))
	if block == "" {
		t.Fatal("没在 webui.go 里找到 SERVICES 定义")
	}
	var jsQuota []string
	for _, line := range strings.Split(block, "\n") {
		if m := regexp.MustCompile(`key:'([a-z]+)'`).FindStringSubmatch(line); m != nil {
			if strings.Contains(line, "quota:true") {
				jsQuota = append(jsQuota, m[1])
			}
		}
	}

	want := map[string]bool{}
	for _, k := range creditKeys {
		want[k] = true
	}
	got := map[string]bool{}
	for _, k := range jsQuota {
		got[k] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("后端 creditKeys 有 %q，前端 SERVICES 却没标 quota:true —— 这条线路的额度永远不显示", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("前端给 %q 标了 quota:true，后端却不查它 —— 点刷新只会得到「未知平台」", k)
		}
	}

	// OpenCode 免费档必须明确没有额度入口（用户 2026-10-17 的要求）。
	if want["oc"] || got["oc"] {
		t.Error("oc 不该出现在任何一侧的额度清单里：匿名免费档没有账号额度")
	}
}
