package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
	"time"
)

func try(p *Proxy, h map[string]string, ep, body string) {
	b, s, _ := httpPostJSON(p.Backend+ep, h, []byte(body))
	if zr, e := gzip.NewReader(bytes.NewReader(b)); e == nil {
		b, _ = io.ReadAll(zr)
	}
	fmt.Printf("%s HTTP %d: %s\n", ep, s, truncate(string(b), 150))
}

func TestZZCbConv(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	body := `{"prompt":"API测试对话","model":"hy3","conversationOrigin":"agents"}`
	for _, ep := range []string{
		"/console/as/conversations/",
		"/v2/as/conversations",
		"/as/conversations",
		"/v2/cloudagent/conversations",
	} {
		try(p, h, ep, body)
		time.Sleep(400 * time.Millisecond)
	}
}
