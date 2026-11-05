package main

// 客户端画像的守卫测试（2026-11-05）。
//
// 这张表的用途是「反代谁就装谁」，最容易出的故障是**新增线路忘了登记**，
// 于是它默默用 chrome 默认档、心跳用全局节奏 —— 看起来一切正常，其实少了一维伪装。
// 下面几条测试就是盯这个。

import (
	"strings"
	"testing"
	"time"
)

func TestClientProfilesCoverAllLines(t *testing.T) {
	for _, s := range lineSpecs {
		w, ok := clientWires[s.Key]
		if !ok {
			t.Fatalf("线路 %q 没有登记客户端画像（client_profile.go 的 clientWires）", s.Key)
		}
		if w.TLS == "" {
			t.Errorf("线路 %q 的 TLS 档为空", s.Key)
		}
		if _, ok := tlsFPProfiles[w.TLS]; !ok {
			t.Errorf("线路 %q 的 TLS 档 %q 不在 tlsFPProfiles 里", s.Key, w.TLS)
		}
		// 桌面端 → 浏览器族；CLI → Go 形状。这条不满足通常意味着画像抄错了。
		id, ok := lineIdentityOf(s.Key)
		if !ok {
			t.Fatalf("线路 %q 没在 line_identity.go 登记", s.Key)
		}
		if id.Desktop && w.TLS != "chrome" {
			t.Errorf("线路 %q 自称桌面端（%s），TLS 档却是 %q", s.Key, id.Client, w.TLS)
		}
		if !id.Desktop && w.TLS == "chrome" {
			t.Errorf("线路 %q 自称 CLI（%s），不该装浏览器指纹", s.Key, id.Client)
		}
		if w.Heartbeat && (w.HBMin < 60 || w.HBMax <= w.HBMin) {
			t.Errorf("线路 %q 的心跳节奏不合理：%d~%d", s.Key, w.HBMin, w.HBMax)
		}
	}
}

func TestClientProfileDefaultsHTTP1(t *testing.T) {
	for _, k := range []string{"AIHUB_HTTP2", "AIHUB_HTTP2_INTL", "AIHUB_HTTP2_DOM"} {
		t.Setenv(k, "")
	}
	// 社区两个 Go 反代的抓包结论都是 HTTP/1.1（见 client_profile.go 文件头）。
	for _, line := range []string{"dom", "intl", "qoder", "zcode"} {
		if clientProfileFor(line).HTTP2 {
			t.Errorf("%s 默认应走 HTTP/1.1", line)
		}
	}
}

func TestClientProfileHTTP2Override(t *testing.T) {
	t.Setenv("AIHUB_HTTP2", "")
	t.Setenv("AIHUB_HTTP2_INTL", "")
	t.Setenv("AIHUB_HTTP2_DOM", "")

	t.Setenv("AIHUB_HTTP2", "1")
	if !clientProfileFor("intl").HTTP2 || !clientProfileFor("dom").HTTP2 {
		t.Fatal("AIHUB_HTTP2=1 应全局打开 h2")
	}
	// 按线路覆盖优先
	t.Setenv("AIHUB_HTTP2_INTL", "0")
	if clientProfileFor("intl").HTTP2 {
		t.Fatal("AIHUB_HTTP2_INTL=0 应关掉 intl 的 h2")
	}
	if !clientProfileFor("dom").HTTP2 {
		t.Fatal("dom 未覆盖，应沿用全局（开）")
	}
	// 认不出的值不生效（回落默认 false）
	t.Setenv("AIHUB_HTTP2", "maybe")
	t.Setenv("AIHUB_HTTP2_INTL", "maybe")
	if clientProfileFor("intl").HTTP2 {
		t.Fatal("无法识别的值应回落画像默认")
	}
	// 别名写法
	t.Setenv("AIHUB_HTTP2", "http1")
	if clientProfileFor("qoder").HTTP2 {
		t.Fatal("http1 应等价于关")
	}
}

func TestHeartbeatDelayUsesClientCadence(t *testing.T) {
	cfg := defaultHeartbeatSettings()
	cfg.envCadence = false

	for i := 0; i < 200; i++ {
		d := heartbeatDelayForLine(cfg, "qoder")
		sec := int(d.Seconds())
		if sec < 300 || sec > 600 {
			t.Fatalf("qoder 心跳间隔应在 300~600s，得到 %d", sec)
		}
	}
	// trae 没有心跳探针：即使传了 trae 也要用全局值（240~540）
	for i := 0; i < 100; i++ {
		sec := int(heartbeatDelayForLine(cfg, "trae").Seconds())
		if sec < 240 || sec > 540 {
			t.Fatalf("trae 应回落全局 240~540s，得到 %d", sec)
		}
	}
	// 未登记线路同理
	sec := int(heartbeatDelayForLine(cfg, "not-a-line").Seconds())
	if sec < 240 || sec > 540 {
		t.Fatalf("未知线路应回落全局，得到 %d", sec)
	}
}

func TestHeartbeatDelayEnvWins(t *testing.T) {
	cfg := defaultHeartbeatSettings()
	cfg.envCadence = true // 模拟 AIHUB_HEARTBEAT_MIN/_MAX 被显式设置
	cfg.MinIntervalSec = 90
	cfg.MaxIntervalSec = 90

	d := heartbeatDelayForLine(cfg, "qoder")
	if d != 90*time.Second {
		t.Fatalf("环境变量显式指定时应优先，得到 %v", d)
	}
}

func TestClientProfilesSummaryOrder(t *testing.T) {
	out := clientProfilesSummary()
	if len(out) != len(lineSpecs) {
		t.Fatalf("画像数量 %d 与线路数 %d 不一致", len(out), len(lineSpecs))
	}
	for i, s := range lineSpecs {
		if out[i].Line != s.Key {
			t.Fatalf("第 %d 个画像应是 %s，得到 %s", i, s.Key, out[i].Line)
		}
		if out[i].Client == "" {
			t.Errorf("%s 的客户端名称为空", s.Key)
		}
		if strings.TrimSpace(out[i].Display) == "" {
			t.Errorf("%s 的展示名为空", s.Key)
		}
	}
}

func TestWBCNBaseEnvOverride(t *testing.T) {
	t.Setenv("AIHUB_WB_CN_BASE", "")
	if got := wbCNBase(); got != "https://www.workbuddy.cn" {
		t.Fatalf("默认 base 应是官方客户端的 endpoint（www.workbuddy.cn），得到 %q", got)
	}
	t.Setenv("AIHUB_WB_CN_BASE", "https://www.workbuddy.cn/")
	if got := wbCNBase(); got != "https://www.workbuddy.cn" {
		t.Fatalf("覆盖值应去掉尾部斜杠，得到 %q", got)
	}
}
