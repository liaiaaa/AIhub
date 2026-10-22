package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
)

func TestZZCbModel2(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	h2 := map[string]string{"Content-Type": "application/json"}
	for k, v := range h {
		h2[k] = v
	}
	b, s, err := httpPostJSON(p.Backend+"/v2/chat/completions", h2, []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}],"stream":false,"max_tokens":8}`))
	out := b
	if zr, e := gzip.NewReader(bytes.NewReader(b)); e == nil {
		out, _ = io.ReadAll(zr)
	}
	fmt.Printf("MODEL HTTP %d err=%v: %s\n", s, err, string(out[:min(300, len(out))]))
}
