package main

// Web 版（/app cloudagent）接口可行性探测：桌面 bearer 能否直接打通。

import (
	"fmt"
	"testing"
)

func TestZZWebAgentProbe(t *testing.T) {
	requireLive(t)
	for _, credName := range []string{"国内workbuddy凭证"} {
		dir := domCredDirs()[credName]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		headers, err := cm.Headers()
		if err != nil {
			t.Fatalf("取头失败: %v", err)
		}
		headers["X-Product-Code"] = "workbuddy"
		fmt.Printf("=== %s ===\n", credName)
		for _, ep := range []string{
			"/v2/user/cloudagent/quota",
			"/v2/user/cloudagent/entitlement",
			"/v2/user/cloudagent/tasks",
			"/v2/as/scheduler/tasks",
			"/v2/chat/busy",
		} {
			b, status, err := httpGetJSON(p.Backend+ep, headers)
			fmt.Printf("  GET %-38s HTTP %d: %s\n", ep, status, truncate(string(b), 250))
			if err != nil {
				fmt.Printf("      err: %v\n", err)
			}
		}
	}
}
