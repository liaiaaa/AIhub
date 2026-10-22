package main

import (
	"fmt"
	"testing"
)

func TestZZExp3(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, ep := range []string{
		"/api/v2/open-platform/experts?pageSize=30&page=1",
		"/v2/user/cloudagent/agents/?pageSize=30",
		"/v2/user/cloudagent/orchestrator",
	} {
		b, s, _ := httpGetJSON(p.Backend+ep, h)
		ds := string(gunzip(b))
		fmt.Printf("=== %s HTTP %d len=%d\n  %s\n", ep, s, len(ds), truncate(ds, 260))
	}
}
