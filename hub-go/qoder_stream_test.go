package main

// 首帧上游错误检测测试：provider_error 的自动重试依赖它，漏判 = 重试永不触发。
//
// 2026-09-26 教训：网关把错误帧包在信封里（{"headers":...,"body":"{\"code\":...}"})，
// 旧实现只解析裸帧 → 「首帧 provider_error 重试」从未生效，用户直接看到
// 「本轮运行失败 上游网关错误 provider_error」。本文件锁定包装形态的识别。

import (
	"testing"
)

// 回归（2026-09-26）：DSH 在会话中途切换模型时插入 {"role":"system","content":[]}，
// Qoder 网关对它返回 provider_error 且重试无效。空 system 必须在转发前剔除；
// 空 assistant（纯 tool_calls 帧）要保留。
func TestNormalizeMessagesDropsEmptySystem(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "real system"},
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": []any{}},
		map[string]any{"role": "system", "content": ""},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{}},
		map[string]any{"role": "developer", "content": "dev note"},
	}
	out := qoderNormalizeMessages(msgs)
	if len(out) != 4 {
		t.Fatalf("应剔除 2 条空 system, 实际剩 %d 条", len(out))
	}
	roles := make([]string, 0, len(out))
	for _, m := range out {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	want := "system,user,assistant,system"
	if got := joinRoles(roles); got != want {
		t.Errorf("角色序列 = %q, want %q（空 assistant 保留）", got, want)
	}
}

func joinRoles(r []string) string {
	s := ""
	for i, v := range r {
		if i > 0 {
			s += ","
		}
		s += v
	}
	return s
}

func TestFirstFrameUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		head string
		want bool
	}{
		{"裸错误帧", "data: {\"code\":\"provider_error\",\"message\":\"Error in upstream response\"}\n\n", true},
		{"包装错误帧", "data: {\"headers\":{},\"body\":\"{\\\"code\\\":\\\"provider_error\\\",\\\"message\\\":\\\"Error in upstream response\\\"}\",\"statusCodeValue\":200}\n\n", true},
		{"错误帧前有空壳包装帧", "data: {\"headers\":{},\"body\":\"\"}\ndata: {\"headers\":{},\"body\":\"{\\\"code\\\":\\\"400\\\",\\\"message\\\":\\\"[FAIL]boom\\\"}\"}\n\n", true},
		{"正常裸chunk先行", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"code\":\"provider_error\",\"message\":\"late\"}\n\n", false},
		{"正常包装chunk先行", "data: {\"headers\":{},\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"hi\\\"}}]}\"}\n\n", false},
		{"DONE先行", "data: {\"headers\":{},\"body\":\"[DONE]\"}\n\n", false},
		{"半行不完整", "data: {\"code\":\"pro", false},
		{"空输入", "", false},
		{"无data行", "event: ping\n\n", false},
	}
	for _, c := range cases {
		if got := firstFrameUpstreamError([]byte(c.head)); got != c.want {
			t.Errorf("%s: firstFrameUpstreamError = %v, want %v", c.name, got, c.want)
		}
	}
}
