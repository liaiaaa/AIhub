package main

import (
	"fmt"
	"testing"
)

func TestZZCloudAgent(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()
	headers["X-Product-Code"] = "workbuddy"
	for _, ep := range []string{
		"/v2/user/cloudagent/orchestrator",
		"/v2/as/netdrive/cloudagent-config/download",
		"/v2/user/cloudagent/agents/templates?limit=3",
	} {
		b, s, err := httpGetJSON(p.Backend+ep, headers)
		fmt.Printf("GET %s HTTP %d: %s (e=%v)\n", ep, s, truncate(string(b), 300), err)
	}
}
