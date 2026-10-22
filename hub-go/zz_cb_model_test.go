package main

import (
	"fmt"
	"testing"
	"time"
)

func TestZZCbModel(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	body := `{"model":"hy3","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":8}`
	for _, ep := range []string{"/v2/chat/completions", "/api/v3/chat/completions"} {
		b, s, _ := httpPostJSON(p.Backend+ep, h, []byte(body))
		fmt.Printf("%s HTTP %d: %s\n", ep, s, truncate(string(b), 240))
		time.Sleep(500 * time.Millisecond)
	}
}
