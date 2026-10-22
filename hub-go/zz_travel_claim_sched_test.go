package main

// 到点领取旅行奖励：arrived 才 claim，并显示各记录实际到账积分。

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZTravelClaimScheduled(t *testing.T) {
	for _, credName := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[credName]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		headers, err := cm.Headers()
		if err != nil {
			fmt.Printf("=== %s 取头失败 %v\n", credName, err)
			continue
		}
		st, _, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", headers)
		var env struct {
			Data struct {
				State       string `json:"state"`
				RewardCredit int   `json:"reward_credit"`
			} `json:"data"`
		}
		json.Unmarshal(st, &env)
		fmt.Printf("=== %s: state=%s 预期积分=%d\n", credName, env.Data.State, env.Data.RewardCredit)
		if env.Data.State == "arrived" {
			b, status, err := httpPostJSON(p.Backend+"/activity/growth/buddy/travel/claim", headers, []byte("{}"))
			fmt.Printf("    claim HTTP %d: %s (err=%v)\n", status, truncate(string(b), 300), err)
		}
		// 最近记录里各自的 reward_credit
		rc, _, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/records?limit=20", headers)
		var rv struct {
			Data struct {
				Records []struct {
					RecordID     int64 `json:"record_id"`
					RewardCredit int   `json:"reward_credit"`
					Location     struct {
						Name string `json:"name"`
					} `json:"location"`
				} `json:"records"`
			} `json:"data"`
		}
		json.Unmarshal(rc, &rv)
		for i, r := range rv.Data.Records {
			if i >= 3 {
				break
			}
			fmt.Printf("    记录 %d %s reward=%d\n", r.RecordID, r.Location.Name, r.RewardCredit)
		}
	}
}
