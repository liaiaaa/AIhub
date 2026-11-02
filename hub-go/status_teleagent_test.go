package main

// 2026-11-02：TeleAgent 加进 statusMap 的回归。
//
// 起因：线路能启动（toast 提示"已启动"），但控制台磁贴一直显示"未启动"——
// 因为 statusMap 是逐线路手写的 map，新增线路没加进去，前端读不到 running 状态。
// 这个测试保证「lineSpecs 里的每条线路都在 statusMap 里有条目」，以后再加线路
// 漏掉就会红。

import "testing"

func TestStatusMapCoversEveryLineSpec(t *testing.T) {
	h := &Hub{}
	st := h.statusMap()
	for _, spec := range lineSpecs {
		v, ok := st[spec.Key]
		if !ok {
			t.Errorf("statusMap 缺线路 %q（前端磁贴会一直显示未启动）", spec.Key)
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%q 的值类型不是 map：%T", spec.Key, v)
			continue
		}
		if _, has := m["up"]; !has {
			t.Errorf("%q 缺 up 字段", spec.Key)
		}
	}
	if got, ok := st["teleagent"].(map[string]any); !ok {
		t.Fatal("teleagent 条目类型不对")
	} else if got["up"] != false {
		t.Errorf("未启动的线路 up 应为 false，实际 %v", got["up"])
	}
}

func TestConsoleAggLineHasTeleagent(t *testing.T) {
	src := consoleSource(t)
	want := `<option value="teleagent">TeleAgent</option>`
	if !contains(src, want) {
		t.Errorf("模型添加的线路下拉缺 %q（前端就选不到这条线路）", want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
