package main

import (
	"fmt"
	"testing"
)

func TestZZBal2(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, ep := range []string{
		"/billing/meter/get-user-resource-summary",
		"/billing/meter/get-user-resource-free-packages",
	} {
		b, s, err := httpGetJSON(p.Backend+ep, h)
		fmt.Printf("GET %s HTTP %d: %s (e=%v)\n", ep, s, truncate(string(b), 600), err)
	}
}
