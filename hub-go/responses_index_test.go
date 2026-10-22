package main

// 2026-10-21 开源审查修的 P1：Responses 流式里 output_item.added / done 的
// output_index 必须一致。此前 handleToolCall 从不递增 st.itemIndex，closeTools
// 却每条 +1 —— 一次响应出现 ≥2 个 tool_call 时 done 找不到对应的 added，
// Codex 侧工具调用被丢弃或错配。

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 从 SSE 文本里按出现顺序抽出 (event, output_index)。
func parseItemIndexes(body string) [][2]string {
	var out [][2]string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := line[6:]
		if !strings.Contains(d, `"output_index"`) {
			continue
		}
		ev := ""
		// 往上找最近一行 event:
		out = append(out, [2]string{ev, d})
	}
	return out
}

func TestResponsesToolOutputIndexConsistency(t *testing.T) {
	st := &respStreamState{toolSlots: map[int]*respToolSlot{}}
	rr := httptest.NewRecorder()

	// 三个并行 tool_call，且出现在文本之前（纯工具调用响应的典型形态）
	for i := 0; i < 3; i++ {
		st.handleToolCall(rr, map[string]any{
			// intVal 只认 json.Number / float64（真实请求经 JSON 解码就是这两种）
			"index": json.Number(strconv.Itoa(i)),
			"id":    "call_" + string(rune('a'+i)),
			"function": map[string]any{
				"name":      "bash",
				"arguments": `{"command":"ls"}`,
			},
		})
	}
	st.closeTools(rr)

	body := rr.Body.String()
	// added 与 done 各 3 条，output_index 集合必须都等于 {0,1,2}
	var added, done []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		d := line[6:]
		i := strings.Index(d, `"output_index":`)
		if i < 0 {
			continue
		}
		rest := d[i+len(`"output_index":`):]
		end := strings.IndexAny(rest, ",}")
		if end < 0 {
			continue
		}
		v := strings.TrimSpace(rest[:end])
		if strings.Contains(d, `"response.output_item.added"`) {
			added = append(added, v)
		} else if strings.Contains(d, `"response.output_item.done"`) {
			done = append(done, v)
		}
	}
	if len(added) != 3 || len(done) != 3 {
		t.Fatalf("added/done 事件数不对：added=%v done=%v", added, done)
	}
	for i := range added {
		if added[i] != done[i] {
			t.Errorf("第 %d 个工具 added=%s 但 done=%s —— index 对不上，客户端会丢弃该工具调用",
				i, added[i], done[i])
		}
	}
	// 三个工具必须是不同下标
	seen := map[string]bool{}
	for _, v := range added {
		if seen[v] {
			t.Errorf("两个工具拿到同一个 output_index %s", v)
		}
		seen[v] = true
	}
}

// 文本 + 工具混排时，下标也必须各占一位、不重叠。
func TestResponsesTextThenToolsIndexes(t *testing.T) {
	st := &respStreamState{toolSlots: map[int]*respToolSlot{}}
	rr := httptest.NewRecorder()

	st.textBuf.WriteString("先说明一下")
	st.msgAnnounced = true
	st.msgID = "msg_1"
	st.closeMessage(rr) // 占 index 0
	st.handleToolCall(rr, map[string]any{
		"index": json.Number("0"), "id": "call_x",
		"function": map[string]any{"name": "bash", "arguments": "{}"},
	})
	st.closeTools(rr)
	if st.itemIndex != 2 {
		t.Errorf("一个 message + 一个工具应占用 2 个下标，实际 itemIndex=%d", st.itemIndex)
	}
}
