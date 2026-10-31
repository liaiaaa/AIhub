package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestZZTplStat(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/?limit=20", h)
	var r struct {
		Data struct {
			Conversations []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				ID     string `json:"id"`
			} `json:"conversations"`
		} `json:"data"`
	}
	json.Unmarshal(gunzip(b), &r)
	for _, c := range r.Data.Conversations {
		if strings.Contains(c.Name, "回复一个字") || c.Status != "completed" {
			fmt.Printf("%-14s %s\n", c.Name, c.Status)
		}
	}
}
