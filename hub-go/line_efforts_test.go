package main

// 2026-11-03：统一思考档位的映射与钳制。

import (
	"reflect"
	"testing"
)

func TestCanonicalizeTraeLevels(t *testing.T) {
	// trae 原生 light/high/extra_high → 规范 low/high/max
	got := canonicalizeEfforts("trae", []string{"light", "high", "extra_high"})
	want := []string{"low", "high", "max"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("trae 规范档 = %v，期望 %v", got, want)
	}
}

func TestCanonicalizeDropsUnknown(t *testing.T) {
	// 认不出的档位必须丢掉（宁缺不编）
	got := canonicalizeEfforts("dom", []string{"low", "turbo", "max"})
	want := []string{"low", "max"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("未知档位应被丢弃，得到 %v", got)
	}
}

func TestCanonicalizeSortsByStrength(t *testing.T) {
	got := canonicalizeEfforts("qoder", []string{"high", "low", "medium"})
	want := []string{"low", "medium", "high"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("应按强度升序，得到 %v", got)
	}
}

func TestCanonicalToWire(t *testing.T) {
	cases := []struct {
		line, want, wire string
		ok               bool
	}{
		{"trae", "max", "extra_high", true},
		{"trae", "medium", "high", true},
		{"trae", "minimal", "light", true},
		{"trae", "extra_high", "extra_high", true}, // 客户端发原生名也放行
		{"trae", "turbo", "", false},
		{"zcode", "minimal", "low", true},
		{"zcode", "xhigh", "max", true},
		{"zcode", "medium", "high", true},
		{"dom", "max", "max", true},
		{"dom", "Max", "max", true}, // 大小写不敏感
		{"dom", "turbo", "", false},
	}
	for _, c := range cases {
		got, ok := canonicalToWireEffort(c.line, c.want)
		if ok != c.ok || got != c.wire {
			t.Errorf("canonicalToWireEffort(%q,%q) = (%q,%v)，期望 (%q,%v)",
				c.line, c.want, got, ok, c.wire, c.ok)
		}
	}
}

func TestWireToCanonical(t *testing.T) {
	if got := wireToCanonicalEffort("trae", "extra_high"); got != "max" {
		t.Errorf("extra_high 应还原为 max，实际 %q", got)
	}
	if got := wireToCanonicalEffort("dom", "high"); got != "high" {
		t.Errorf("恒等映射失败：%q", got)
	}
}

func TestClampEffort(t *testing.T) {
	levels := []string{"low", "high", "max"}
	if got := clampEffort(levels, "medium"); got != "low" && got != "high" {
		t.Errorf("medium 应钳到 low 或 high，实际 %q", got)
	}
	if got := clampEffort(levels, "max"); got != "max" {
		t.Errorf("max 应原样保留，实际 %q", got)
	}
	if got := clampEffort(levels, "turbo"); got != "max" {
		t.Errorf("未知档应钳到最高档，实际 %q", got)
	}
}

func TestEffortsOfTraeModel(t *testing.T) {
	h := &Hub{}
	le, ok := h.effortsOf("trae", "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("trae deepseek-v4.1-flash 应有档位")
	}
	if len(le.Levels) != 3 || le.Default != "max" {
		t.Errorf("trae 档位 = %+v，期望 3 档且默认 max", le)
	}
	// 不支持思考的模型（mimo-v2.6-flash 在 trae 表里是 nil）
	if _, ok := h.effortsOf("trae", "mimo-v2.6-flash"); ok {
		t.Error("trae 的 mimo-v2.6-flash 官方 support_thinking=false，不应有档位")
	}
}

func TestEffortsOfTeleagent(t *testing.T) {
	h := &Hub{}
	le, ok := h.effortsOf("teleagent", "chat-pro")
	if !ok {
		t.Fatal("teleagent 实测支持 low/high/max，应宣告档位")
	}
	if len(le.Levels) != 3 || le.Levels[0] != "low" || le.Levels[2] != "max" {
		t.Errorf("teleagent 档位 = %v，期望 [low high max]", le.Levels)
	}
	if le.Default != "max" {
		t.Errorf("teleagent 默认档应为 max（实测不传时思考最深），实际 %q", le.Default)
	}
	// 上游拒绝「不思考」，所以 minimal 必须折到 low，不能原样发过去
	if w, ok := canonicalToWireEffort("teleagent", "minimal"); !ok || w != "low" {
		t.Errorf("teleagent minimal 应折到 low，实际 (%q,%v)", w, ok)
	}
}

func TestEffortsOfMimoNotAnnounced(t *testing.T) {
	h := &Hub{}
	// mimo 本机没登录数据，未能实测 → 不宣告
	if _, ok := h.effortsOf("mimo", "MiMo-V2-Flash"); ok {
		t.Error("mimo 未验证前不应宣告档位")
	}
}
