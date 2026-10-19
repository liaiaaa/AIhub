package main

// 2026-10-19 用户报「用量面板缓存命中计算错误」。
//
// 面板显示 48.9%，实际应为 95.8%。根因是前端把命中率算成
//   cached / (in + cached)
// 而 OpenAI 语义里 prompt_tokens_details.cached_tokens 是 prompt_tokens 的
// **子集**，正确算法是
//   cached / in
//
// 实测证据（usage.jsonl 全量 19958 条带 cached 的记录）：
//   cached <= in 的：19958 条
//   cached >  in 的：0 条          ← cached 从不是独立于 in 的额外量
//   单请求抽样命中率：97.0% / 99.0% / 99.2% / 97.6% / 99.2%
//   汇总：in=3909623323 cached=3746632385
//     错误算法 → 48.9%   正确算法 → 95.8%

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// usageFrom 必须把 cached 采集自 prompt_tokens_details.cached_tokens
// （in 的子集），这是命中率算法正确的前提。
func TestUsageCachedIsSubsetOfInput(t *testing.T) {
	u := map[string]any{
		"prompt_tokens":     float64(100000),
		"completion_tokens": float64(500),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(95000),
		},
	}
	in, _, cached, _, _ := usageFrom(u)
	if in != 100000 {
		t.Fatalf("in = %d，应为 100000", in)
	}
	if cached != 95000 {
		t.Fatalf("cached = %d，应为 95000", cached)
	}
	// 这条是算法的地基：cached 不能超过 in
	if cached > in {
		t.Fatalf("cached(%d) > in(%d)：采集语义变了，命中率算法要重新审", cached, in)
	}
}

// 真实数据里 cached 从未超过 in —— 用磁盘上的 usage.jsonl 复验（无文件则跳过）。
func TestRealUsageDataCachedNeverExceedsInput(t *testing.T) {
	f, err := os.Open(dataPath("usage.jsonl"))
	if err != nil {
		t.Skip("没有 usage.jsonl，跳过")
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	total, checked := 0, 0
	for {
		var r usageRec
		if err := dec.Decode(&r); err != nil {
			break
		}
		total++
		if r.In > 0 && r.Cached > 0 {
			checked++
			if r.Cached > r.In {
				t.Fatalf("发现 cached(%d) > in(%d) 的记录，命中率算法假设被打破", r.Cached, r.In)
			}
		}
	}
	if total == 0 {
		t.Skip("usage.jsonl 为空")
	}
	t.Logf("检查 %d 条记录，其中 %d 条同时有 in 与 cached，无一条 cached > in", total, checked)
}

// 前端命中率算法必须写成 cached/in，不能是 cached/(in+cached)。
// 用源码文本断言：这段逻辑在 webui.go 的模板字符串里，跑不了单测。
func TestCacheHitFormulaUsesCachedOverInput(t *testing.T) {
	b, err := os.ReadFile("webui.go")
	if err != nil {
		t.Skipf("读不到 webui.go：%v", err)
	}
	s := string(b)
	if strings.Contains(s, "s.cached / (s.in + s.cached)") {
		t.Fatal("命中率仍用 cached/(in+cached)：cached 是 in 的子集，" +
			"分母重复相加会让命中率被腰斩（实测 95.8% 显示成 48.9%）")
	}
	if !strings.Contains(s, "s.cached / s.in") {
		t.Error("没找到正确的命中率公式 cached/in")
	}
}

// 2026-10-19 第二个 bug：总 tokens 把 reasoning 加了两遍。
//
// 面板原来算 total = in + out + reason。但 reason 取自
// completion_tokens_details.reasoning_tokens，是 completion_tokens(=out) 的子集。
// 全量实测 20881 条记录：total == in+out 无一例外（0 条不等），
// 样本 7257+198=7455 正确，而 in+out+reason=7548 多算。
func TestUsageReasonIsSubsetOfOutput(t *testing.T) {
	u := map[string]any{
		"prompt_tokens":     float64(7257),
		"completion_tokens": float64(198),
		"total_tokens":      float64(7455),
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": float64(93),
		},
	}
	in, out, _, reason, total := usageFrom(u)
	if total != in+out {
		t.Errorf("采集到的 total(%d) 应等于 in+out(%d)", total, in+out)
	}
	if reason > out {
		t.Errorf("reason(%d) > out(%d)：reasoning 是 out 的子集，面板不能把它再加一遍",
			reason, out)
	}
}

// 真实数据复验：total == in+out 必须成立（无 total 的记录跳过）。
func TestRealUsageTotalEqualsInPlusOut(t *testing.T) {
	f, err := os.Open(dataPath("usage.jsonl"))
	if err != nil {
		t.Skip("没有 usage.jsonl，跳过")
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	checked := 0
	for {
		var r usageRec
		if err := dec.Decode(&r); err != nil {
			break
		}
		if r.Total == 0 {
			continue
		}
		checked++
		if r.Total != r.In+r.Out {
			t.Fatalf("记录 total(%d) != in+out(%d)：面板用 in+out 的假设被打破",
				r.Total, r.In+r.Out)
		}
	}
	if checked == 0 {
		t.Skip("没有带 total 的记录")
	}
	t.Logf("检查 %d 条带 total 的记录，全部满足 total == in+out", checked)
}

// 前端总 tokens 公式必须是 in+out，不能加 reason。
func TestTotalTokensFormulaExcludesReason(t *testing.T) {
	b, err := os.ReadFile("webui.go")
	if err != nil {
		t.Skipf("读不到 webui.go：%v", err)
	}
	s := string(b)
	if strings.Contains(s, "const total = s.in + s.out + s.reason;") {
		t.Fatal("总 tokens 仍把 reason 加了两遍（reason 是 out 的子集）")
	}
	if !strings.Contains(s, "const total = s.in + s.out;") {
		t.Error("没找到正确的总 tokens 公式 in+out")
	}
}
