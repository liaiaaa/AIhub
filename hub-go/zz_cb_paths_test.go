package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestZZCbPaths(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, ep := range []string{
		"/console/as/experts?limit=5",
		"/console/experts?limit=5",
		"/v2/experts?limit=5",
		"/console/as/scenes",
		"/console/as/quick-actions",
		"/v2/operation/quick-actions",
		"/console/as/skills?limit=5",
	} {
		b, s, _ := httpGetJSON(p.Backend+ep, h)
		if zr, e := gzip.NewReader(bytes.NewReader(b)); e == nil {
			b, _ = io.ReadAll(zr)
		}
		if s != 404 {
			fmt.Printf("%s HTTP %d: %s\n", ep, s, truncate(string(b), 130))
		} else {
			fmt.Printf("%s 404\n", ep)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
