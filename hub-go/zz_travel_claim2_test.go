package main

import (
	"fmt"
	"testing"
)

func TestZZTravelClaim2(t *testing.T) {
	for _, cred := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[cred]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		h, _ := cm.Headers()
		b, s, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", h)
		fmt.Printf("%s status HTTP %d: %s\n", cred, s, truncate(string(gunzip(b)), 130))
	}
}
