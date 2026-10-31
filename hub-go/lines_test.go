package main

// lineSpecs 的等价性测试（2026-10-31 A2 第一步）。
//
// 目的：把「元数据收敛成一张表」这件事钉死成可回归的断言。
// 这些数字全部来自收敛前的字面量（main.go 的 DomPort/IntlPort、
// proxy_qoder.go:48、proxy_zcode.go:33、trae_proxy.go:44、
// proxy_mimo.go:14、opencode_proxy.go:55、proxy_agg.go:71 的 aggLinePort，
// 以及 hub.go:123 的 knownLineNames），改动必须显式改这里。

import "testing"

// TestLineSpecsPortsUnchanged 端口与收敛前逐条一致。
func TestLineSpecsPortsUnchanged(t *testing.T) {
	want := map[string]int{
		"dom":   8787,
		"intl":  8799,
		"qoder": 8797,
		"trae":  8795,
		"zcode": 8796,
		"mimo":  8798,
		"oc":    8800,
	}
	got := make(map[string]int, len(lineSpecs))
	for _, s := range lineSpecs {
		got[s.Key] = s.Port
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("线路 %s 端口 = %d，收敛前是 %d", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("线路条数 = %d，收敛前是 %d", len(got), len(want))
	}
}

// TestLineKeysOrderUnchanged 线路顺序与 knownLineNames 收敛前一致。
func TestLineKeysOrderUnchanged(t *testing.T) {
	want := []string{"dom", "intl", "qoder", "trae", "zcode", "mimo", "oc"}
	got := lineKeys()
	if len(got) != len(want) {
		t.Fatalf("线路名条数 = %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个线路名 = %q，期望 %q", i, got[i], want[i])
		}
	}
	if len(knownLineNames) != len(want) {
		t.Errorf("knownLineNames 条数 = %d，期望 %d", len(knownLineNames), len(want))
	}
}

// TestAggregatedLinePortsUnchanged 参与聚合的线路集合与端口，与收敛前 aggLinePort 一致
// （注意：mimo 不参与聚合，收敛前后都不能出现在这里）。
func TestAggregatedLinePortsUnchanged(t *testing.T) {
	want := map[string]int{"dom": 8787, "intl": 8799, "qoder": 8797, "trae": 8795, "zcode": 8796, "oc": 8800}
	got := aggregatedLinePorts()
	if len(got) != len(want) {
		t.Fatalf("聚合线路条数 = %d，期望 %d（mimo 不应参与聚合）", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("聚合线路 %s 端口 = %d，期望 %d", k, got[k], v)
		}
	}
	if _, ok := got["mimo"]; ok {
		t.Error("mimo 不应参与 :8790 聚合")
	}
}

// TestAllLinePortsDistinct 清扫端口清单覆盖全部线路且不重复。
func TestAllLinePortsDistinct(t *testing.T) {
	ports := allLinePorts()
	if len(ports) != len(lineSpecs) {
		t.Fatalf("端口清单条数 = %d，期望 %d", len(ports), len(lineSpecs))
	}
	seen := map[int]bool{}
	for _, p := range ports {
		if seen[p] {
			t.Errorf("端口 %d 重复", p)
		}
		seen[p] = true
	}
}

// TestLineSpecsMetadataSane 显示名/日志文件名非空且不重复；dom 必须带高风险标记。
func TestLineSpecsMetadataSane(t *testing.T) {
	names := map[string]bool{}
	logs := map[string]bool{}
	for _, s := range lineSpecs {
		if s.Display == "" {
			t.Errorf("线路 %s 缺显示名", s.Key)
		}
		if names[s.Display] {
			t.Errorf("显示名重复：%s", s.Display)
		}
		names[s.Display] = true
		if s.LogFile == "" || logs[s.LogFile] {
			t.Errorf("线路 %s 的日志文件名缺失或重复：%q", s.Key, s.LogFile)
		}
		logs[s.LogFile] = true
	}
	for _, s := range lineSpecs {
		if s.Key == "dom" && s.Risk == "" {
			t.Error("dom 线路必须标记为高风险（上游 2026-10-23 起收紧非官方客户端识别）")
		}
	}
}
