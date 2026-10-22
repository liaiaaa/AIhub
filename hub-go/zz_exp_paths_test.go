package main

import (
	"fmt"
	"regexp"
	"testing"
	"time"
)

func TestZZExpPaths(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, ep := range []string{
		"/v2/as/experts?limit=10",
		"/v2/expert/list?limit=10",
		"/v2/as/expert/list?page=1&size=10",
		"/v2/as/support/experts",
		"/console/as/support/experts",
	} {
		b, s, _ := httpGetJSON(p.Backend+ep, h)
		ds := string(gunzip(b))
		head := ds
		if len(head) > 110 {
			head = head[:110]
		}
		fmt.Printf("%s HTTP %d: %s\n", ep, s, head)
		if s == 200 && regexp.MustCompile(`ex_|expert`).MatchString(ds) {
			fmt.Println("  >>> 命中!")
		}
		time.Sleep(250 * time.Millisecond)
	}
}
