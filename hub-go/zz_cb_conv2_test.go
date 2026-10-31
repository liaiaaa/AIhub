package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
	"time"
)

func getz(p *Proxy, h map[string]string, ep string) string {
	b, s, _ := httpGetJSON(p.Backend+ep, h)
	if zr, e := gzip.NewReader(bytes.NewReader(b)); e == nil {
		b, _ = io.ReadAll(zr)
	}
	return fmt.Sprintf("HTTP %d: %s", s, truncate(string(b), 400))
}

func TestZZCbConv2(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	time.Sleep(8 * time.Second)
	fmt.Println("列表:", getz(p, h, "/console/as/conversations/?limit=3"))
	fmt.Println("详情:", getz(p, h, "/console/as/conversations/2112803748473638912"))
}
