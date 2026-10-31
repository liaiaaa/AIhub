package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
)

func TestZZCbRisk2(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, s, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", h)
	out := b
	if _, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
		zr, _ := gzip.NewReader(bytes.NewReader(b))
		out, _ = io.ReadAll(zr)
	}
	fmt.Printf("tasks HTTP %d len=%d: %s\n", s, len(out), truncate(string(out), 260))
}
