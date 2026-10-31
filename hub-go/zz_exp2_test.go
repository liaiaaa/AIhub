package main

import (
	"fmt"
	"regexp"
	"testing"
)

func TestZZExp2(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, ep := range []string{
		"/v2/open-platform/experts?page=1&pageSize=20",
		"/console/expert/ranking",
	} {
		b, s, _ := httpGetJSON(p.Backend+ep, h)
		ds := string(gunzip(b))
		fmt.Printf("=== %s HTTP %d len=%d\n", ep, s, len(ds))
		if len(ds) > 80 {
			fmt.Println("  样本:", truncate(ds, 220))
			re := regexp.MustCompile(`"(id|expertId|expert_id)":\s*"?(ex_[A-Za-z0-9]+|[A-Za-z0-9]{8,})"?[^}]{0,80}?"(name|title|displayName)":\s*"([^"]{2,16})"`)
			n := 0
			for _, m := range re.FindAllStringSubmatch(ds, -1) {
				fmt.Printf("   %s = %s\n", m[1], m[3])
				n++
				if n > 8 {
					break
				}
			}
		}
	}
}
