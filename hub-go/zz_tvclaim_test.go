package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZTvClaim(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", h)
	var r struct {
		Data struct {
			RecordID json.Number `json:"record_id"`
		} `json:"data"`
	}
	json.Unmarshal(gunzip(b), &r)
	fmt.Println("record:", r.Data.RecordID)
	c, s, _ := httpPostJSON(p.Backend+"/activity/growth/buddy/travel/claim", h, []byte(fmt.Sprintf(`{"record_id":%s}`, r.Data.RecordID)))
	fmt.Printf("claim HTTP %d: %s\n", s, truncate(string(gunzip(c)), 160))
}
