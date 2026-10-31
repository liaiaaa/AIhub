package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZDaily(t *testing.T) {
	requireLive(t)
	for _, cred := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[cred]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		h, err := cm.Headers()
		if err != nil {
			fmt.Printf("%s 刷新失败: %v\n", cred, err)
			continue
		}
		// 签到状态
		b1, s1, _ := httpGetJSON(p.Backend+"/activity/growth/checkin/status", h)
		// 旅行状态
		b2, s2, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", h)
		fmt.Printf("=== %s === HTTP(%d,%d)\n  checkin: %s\n  travel: %s\n", cred, s1, s2,
			truncate(string(b1), 160), truncate(string(b2), 160))
		var x map[string]any
		json.Unmarshal(b1, &x)
	}
}
