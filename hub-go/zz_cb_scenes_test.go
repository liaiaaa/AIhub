package main

import (
	"fmt"
	"regexp"
	"testing"
	"time"
)

func TestZZCbScenes(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for _, ep := range []string{"/v2/as/support/scenes", "/console/as/support/scenes"} {
		b, s, _ := httpGetJSON(p.Backend+ep, h)
		ds := string(gunzip(b))
		fmt.Printf("%s HTTP %d len=%d\n", ep, s, len(ds))
		if s == 200 && len(ds) > 100 {
			re := regexp.MustCompile(`"id":\s*"?(\d{8,13})"?[^}]{0,120}?"(?:name|title)":\s*"([^"]{1,14})"`)
			n := 0
			for _, m := range re.FindAllStringSubmatch(ds, -1) {
				fmt.Printf("  %s = %s\n", m[1], m[2])
				n++
				if n > 14 {
					break
				}
			}
			if n == 0 {
				fmt.Println("  样本:", truncate(ds, 200))
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
}
