package main

import (
	"fmt"
	"testing"
)

func TestZZOpExp(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, s, _ := httpGetJSON("https://www.workbuddy.cn/v2/open-platform/experts?pageSize=20&page=1", h)
	fmt.Printf("www HTTP %d: %s\n", s, truncate(string(gunzip(b)), 200))
}
