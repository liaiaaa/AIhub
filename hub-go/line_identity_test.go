package main

// 2026-11-03：出站身份总表的完整性 —— 新线路忘了登记会红，
// 「定义了身份但 UA 是空的」也会红（teleagent 那个头空了两个月就是没人盯）。

import (
	"strings"
	"testing"
)

func TestLineIdentitiesCoverEveryLine(t *testing.T) {
	ids := lineIdentities()
	for _, s := range lineSpecs {
		id, ok := ids[s.Key]
		if !ok {
			t.Errorf("线路 %s 没有登记出站身份（line_identity.go）", s.Key)
			continue
		}
		if id.Client == "" {
			t.Errorf("%s 的 Client 为空", s.Key)
		}
		if strings.TrimSpace(id.UserAgent) == "" {
			t.Errorf("%s 没有 UA —— 伪装成官方客户端必须有 UA", s.Key)
		}
		if id.Note == "" {
			t.Errorf("%s 缺 Note（写清依据，便于下次核对）", s.Key)
		}
	}
	// 反过来：登记了但 lineSpecs 里没有的，也算脏数据
	known := map[string]bool{}
	for _, s := range lineSpecs {
		known[s.Key] = true
	}
	for k := range ids {
		if !known[k] {
			t.Errorf("身份表里的 %s 不是已知线路", k)
		}
	}
}

func TestDesktopIdentityExpectations(t *testing.T) {
	// 这些线路必须是「桌面端」伪装
	for _, k := range []string{"dom", "intl", "qoder", "trae", "zcode", "teleagent"} {
		id, ok := lineIdentityOf(k)
		if !ok || !id.Desktop {
			t.Errorf("%s 应为桌面端身份", k)
		}
	}
	// oc 官方只有 CLI，不该硬装桌面端
	if id, _ := lineIdentityOf("oc"); id.Desktop {
		t.Error("oc 官方只有 CLI，不该标记为桌面端")
	}
	// qoder 的 UA 必须是 Qoder（上游按 UA 判定客户端）
	if id, _ := lineIdentityOf("qoder"); !strings.Contains(id.UserAgent, "Qoder") {
		t.Errorf("qoder UA 应为 Qoder，实际 %q", id.UserAgent)
	}
}

func TestIdentitySummaryForAPI(t *testing.T) {
	m := identitySummary("qoder")
	if m == nil {
		t.Fatal("qoder 摘要为空")
	}
	for _, k := range []string{"client", "version", "desktop", "ua", "note"} {
		if _, ok := m[k]; !ok {
			t.Errorf("摘要缺字段 %s", k)
		}
	}
	if identitySummary("不存在") != nil {
		t.Error("未知线路应返回 nil")
	}
}
