package main

// 签到芯片的线路覆盖守卫（2026-11-13）。
//
// 用户报障链：果办接进 runCheckin 后，后端 checkinRecord 有了 hw 字段，
// 但控制台 renderCheckin 的 lines 数组只列了国内/国际/Qoder/Trae 四条 ——
// 界面上看不到果办，用户会以为「没签」。
// 这类「后端加了、前端漏渲染」的漂移只能用测试钉住。

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// TestCheckinChipCoversHuawei 签到芯片必须渲染果办。
func TestCheckinChipCoversHuawei(t *testing.T) {
	src := consoleSource(t)
	// 定位 renderCheckin 函数体
	i := strings.Index(src, "function renderCheckin()")
	if i < 0 {
		t.Fatal("控制台里没找到 renderCheckin")
	}
	body := src[i:]
	if j := strings.Index(body[10:], "function doCheckin"); j > 0 {
		body = body[:10+j]
	}
	if !strings.Contains(body, "c.hw") && !strings.Contains(body, "c && c.hw") {
		t.Error("renderCheckin 没渲染果办（c.hw）：后端签了但界面上看不到")
	}
	if !strings.Contains(body, "'果办'") {
		t.Error("renderCheckin 的线路标签里没有「果办」")
	}
}

// TestCheckinRecordHasHwField checkinRecord 必须持久化果办结果。
func TestCheckinRecordHasHwField(t *testing.T) {
	h := &Hub{}
	h.checkin = checkinRecord{Hw: "果办今日已领"}
	// 走 JSON 往返验证 tag（state.json 持久化靠它）
	b, err := jsonMarshal(h.checkin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"hw"`) {
		t.Errorf("checkinRecord 的 JSON 里没有 hw 字段：%s", string(b))
	}
	if !strings.Contains(string(b), "果办今日已领") {
		t.Errorf("hw 字段值没被序列化：%s", string(b))
	}
}
