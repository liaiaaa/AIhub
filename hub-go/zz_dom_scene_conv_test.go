package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZDomSceneConv(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/?limit=20\u0026page=2", h)
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
		if len([]rune(c.Name)) >= 2 && c.Name[:2] == "使用" {
			d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+c.ID, h)
			fmt.Printf("=== %s (%s)\n%s\n", c.Name, c.ID, truncate(string(gunzip(d)), 700))
			break
		}
	}
}
