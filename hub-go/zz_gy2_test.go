package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestZZGy2(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	for page := 1; page <= 3; page++ {
		b, _, _ := httpGetJSON(p.Backend+fmt.Sprintf("/console/as/conversations/?limit=20&page=%d", page), h)
		var r struct {
			Data struct {
				Conversations []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"conversations"`
			} `json:"data"`
		}
		json.Unmarshal(gunzip(b), &r)
		for _, c := range r.Data.Conversations {
			nm := c.Name
			if strings.Contains(nm, "公益") || strings.Contains(nm, "吉事通") || strings.Contains(nm, "拼豆") {
				d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+c.ID, h)
				ds := string(gunzip(d))
				i := strings.Index(ds, "ex_")
				if i > 0 {
					fmt.Printf("=== %s\n...%s\n", nm, ds[max(0,i-300):i+80])
					return
				}
			}
		}
	}
	fmt.Println("未找到")
}
