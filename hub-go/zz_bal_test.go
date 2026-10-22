package main

import (
	"fmt"
	"testing"
)

func TestZZBalance(t *testing.T) {
	for _, cred := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[cred]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		h, _ := cm.Headers()
		fmt.Printf("=== %s ===\n", cred)
		for _, ep := range []string{"/profile/usage", "/profile/plans-usage", "/activity/growth/energy"} {
			b, s, _ := httpGetJSON(p.Backend+ep, h)
			fmt.Printf("  %s HTTP %d: %s\n", ep, s, truncate(string(b), 300))
		}
	}
}
