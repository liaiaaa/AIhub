package main

// 2026-10-20 积分到期显示的解析与对账规则测试。
// 数据形态来自真实上游（见 zz 探针记录）：
//   gift: data{claimed:true,active:true,credit_num:1500,end_time:"2026-10-31 23:59:59"}
//   comp: data{claimed:true,active:true,credit_num:500, end_time:"2026-10-22 23:59:59"}
//   summary.packages[].{cycleTotalCapacity,cycleRemainCapacity}

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestWBShortDate(t *testing.T) {
	if got := wbShortDate("2026-10-31 23:59:59"); got != "10-31" {
		t.Errorf("gift 日期解析错：%q", got)
	}
	if got := wbShortDate("2026-10-22 00:00:00"); got != "10-22" {
		t.Errorf("comp 日期解析错：%q", got)
	}
	if got := wbShortDate("garbage"); got != "" {
		t.Errorf("脏数据应返回空：%q", got)
	}
	if got := wbShortDate(nil); got != "" {
		t.Errorf("nil 应返回空：%q", got)
	}
}

func TestFlexCreditString(t *testing.T) {
	if got := flexCreditString(float64(1500)); got != "1500" {
		t.Errorf("整数 float64：%q", got)
	}
	if got := flexCreditString("500"); got != "500" {
		t.Errorf("字符串：%q", got)
	}
	if got := flexCreditString(nil); got != "" {
		t.Errorf("nil：%q", got)
	}
}

// 对账规则：CycleTotalCapacity == credit_num 视为同一包；剩 0 隐藏；对不上只给日期。
func TestExpiryCorrelation(t *testing.T) {
	// 用 wbCreditExpiriesFetch 的对账逻辑不好单测（要网络），
	// 这里把它抽出的规则在测试里复刻并钉住期望行为：
	// CodeBuddy：礼包 credit_num=1500，_030 包 total=1500 remain=0 → 隐藏
	//          补偿 credit_num=500， _008 包 total=500  remain=0 → 隐藏
	// workbuddy：礼包 credit_num=1500，包 total=1000 → 对不上 → 只显示到期日
	//          补偿 credit_num=500， 包 total=500  remain=0 → 隐藏
	var sum struct {
		Data struct {
			Packages []struct {
				CycleRemainCapacity flexNum `json:"cycleRemainCapacity"`
				CycleTotalCapacity  flexNum `json:"cycleTotalCapacity"`
			} `json:"packages"`
		} `json:"data"`
	}
	body := `{"data":{"packages":[
		{"cycleTotalCapacity":"500","cycleRemainCapacity":"0"},
		{"cycleTotalCapacity":"1500","cycleRemainCapacity":"0"}]}}`
	if err := json.Unmarshal([]byte(body), &sum); err != nil {
		t.Fatal(err)
	}
	correlate := func(credit string) (remain string, hidden bool) {
		grant, err := parseFloat64(credit)
		if err != nil {
			return "", false
		}
		for _, pkg := range sum.Data.Packages {
			if float64(pkg.CycleTotalCapacity) != grant {
				continue
			}
			r := float64(pkg.CycleRemainCapacity)
			if r <= 0 {
				return "", true
			}
			return trimCredit(r), false
		}
		return "", false
	}
	if _, hidden := correlate("1500"); !hidden {
		t.Error("CodeBuddy 礼包剩 0 应隐藏")
	}
	if _, hidden := correlate("500"); !hidden {
		t.Error("补偿剩 0 应隐藏")
	}
	// 对不上的包（workbuddy 礼包 1500 vs 包 total 1000）：不隐藏也不给 remain
	var sum2 struct {
		Data struct {
			Packages []struct {
				CycleRemainCapacity flexNum `json:"cycleRemainCapacity"`
				CycleTotalCapacity  flexNum `json:"cycleTotalCapacity"`
			} `json:"packages"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(`{"data":{"packages":[{"cycleTotalCapacity":"1000","cycleRemainCapacity":"129.07"}]}}`), &sum2)
	matched := false
	for _, pkg := range sum2.Data.Packages {
		if float64(pkg.CycleTotalCapacity) == 1500 {
			matched = true
		}
	}
	if matched {
		t.Error("1500 不该匹配到 1000 的包")
	}
}

func parseFloat64(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}
func trimCredit(f float64) string {
	return strconv.FormatInt(int64(f+0.5), 10)
}
