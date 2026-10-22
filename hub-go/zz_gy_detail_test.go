package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestZZGyDetail(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/?limit=20&page=2", h)
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
		if !strings.Contains(c.Name, "公益") && !strings.Contains(c.Name, "吉事通") {
			continue
		}
		d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+c.ID, h)
		ds := string(gunzip(d))
		i := strings.Index(ds, "EXPERT")
		if i < 0 {
			i = strings.Index(ds, "expert")
		}
		fmt.Printf("=== %s\n%s\n", c.Name, ds[max(0,i-260):i+160])
		break
	}
}
