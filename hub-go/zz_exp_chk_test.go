package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestZZExpChk(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, id := range []string{"2112000000000000000", "2112000000000000000", "2112000000000000000"} {
		b, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+id, h)
		ds := string(gunzip(b))
		hasX := strings.Contains(ds, "X_EXPERT_ID")
		hasName := strings.Contains(ds, "senior-developer") || strings.Contains(ds, "SeniorDeveloper")
		fmt.Printf("%s X_EXPERT_ID=%v 名字含专家=%v\n", id, hasX, hasName)
	}
}
