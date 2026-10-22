package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZTvDom(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", h)
	var r struct {
		Data struct {
			State    string        `json:"state"`
			RecordID json.Number   `json:"record_id"`
		} `json:"data"`
	}
	json.Unmarshal(gunzip(b), &r)
	fmt.Println("state:", r.Data.State, "record:", r.Data.RecordID)
	if r.Data.State == "arrived" {
		c, s, _ := httpPostJSON(p.Backend+"/activity/growth/buddy/travel/claim", h, []byte(fmt.Sprintf(`{"record_id":%s}`, r.Data.RecordID)))
		fmt.Printf("claim HTTP %d: %s\n", s, truncate(string(gunzip(c)), 120))
	}
}
