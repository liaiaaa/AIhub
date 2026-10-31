package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZTv2(t *testing.T) {
	requireLive(t)
	for _, cred := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[cred]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		h, _ := cm.Headers()
		b, _, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", h)
		var r struct {
			Data struct {
				State    string `json:"state"`
				RecordID int64  `json:"record_id"`
			} `json:"data"`
		}
		json.Unmarshal(gunzip(b), &r)
		fmt.Printf("%s state=%s rec=%d\n", cred, r.Data.State, r.Data.RecordID)
		if r.Data.State == "arrived" {
			c, cs, _ := httpPostJSON(p.Backend+"/activity/growth/buddy/travel/claim", h, []byte(`{}`))
			fmt.Println("  CLAIM:", cs, truncate(string(gunzip(c)), 100))
		}
		d, ds, _ := httpPostJSON(p.Backend+"/activity/growth/buddy/travel/depart", h, []byte(`{"location_id":1}`))
		fmt.Println("  DEPART:", ds, truncate(string(gunzip(d)), 80))
	}
}
