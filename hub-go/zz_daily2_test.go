package main

import (
	"fmt"
	"testing"
)

func TestZZDaily2(t *testing.T) {
	requireLive(t)
	for _, cred := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[cred]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		h, _ := cm.Headers()
		// 签到接口探径
		for _, ep := range []string{"/activity/growth/signin/status", "/activity/growth/checkin", "/activity/growth/streak"} {
			b, s, _ := httpGetJSON(p.Backend+ep, h)
			if s == 200 {
				fmt.Printf("%s %s: %s\n", cred, ep, truncate(string(b), 200))
			}
		}
		// 出发旅行
		b, s, _ := httpPostJSON(p.Backend+"/activity/growth/buddy/travel/depart", h, []byte(`{"location_id":1}`))
		fmt.Printf("%s DEPART HTTP %d: %s\n", cred, s, truncate(string(b), 120))
	}
}
