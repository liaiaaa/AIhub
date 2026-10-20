package main

// 2026-10-20：dsh（OpenAI 新规范）发 role=developer，Trae 上游只认
// ['system','assistant','user','tool','function']，整包拒绝并回
// code=4027「developer is not one of ...」。用户报 Trae 在聚合口和 Codex
// 里都调不通，根因就是它。qoder 侧同款问题在 qoderNormalizeMessages 里
// 已映射 developer→system，这里保持一致。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTraePrepareBodyDeveloperRole(t *testing.T) {
	raw := `{
	  "model":"DeepSeek-V4-Flash",
	  "messages":[
	    {"role":"developer","content":"You are a coding agent."},
	    {"role":"system","content":"sys"},
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":"{}"}}]},
	    {"role":"tool","tool_call_id":"c1","content":"out"},
	    {"role":"developer","content":"second developer msg"}
	  ]
	}`
	var src map[string]any
	if err := json.Unmarshal([]byte(raw), &src); err != nil {
		t.Fatal(err)
	}
	out := traePrepareBody(src, "DeepSeek-V4-Flash")
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 6 {
		t.Fatalf("消息数不该变：6 → %d", len(msgs))
	}
	for i, mi := range msgs {
		m := mi.(map[string]any)
		if role, _ := m["role"].(string); role == "developer" {
			t.Fatalf("#%d 仍是 developer，上游会整包拒绝（code=4027）", i)
		}
	}
	// 两个 developer 都应变成 system，内容保留
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("#0 应为 system，实际 %v", first["role"])
	}
	last := msgs[5].(map[string]any)
	if last["role"] != "system" {
		t.Errorf("#5 应为 system，实际 %v", last["role"])
	}
	// 其他角色不受影响
	if msgs[2].(map[string]any)["role"] != "user" || msgs[3].(map[string]any)["role"] != "assistant" {
		t.Error("user/assistant 角色被误改")
	}
	// 回归对照：qoder 侧的映射仍在（防止有人把两处只改一处）
	qb, err := osReadQoderNormalize()
	if err != nil {
		t.Skipf("读不到 qoder_stream.go：%v", err)
	}
	if !strings.Contains(qb, `role == "developer"`) || !strings.Contains(qb, `"system"`) {
		t.Error("qoderNormalizeMessages 的 developer→system 映射丢了")
	}
}

func osReadQoderNormalize() (string, error) {
	b, err := os.ReadFile("qoder_stream.go")
	return string(b), err
}
