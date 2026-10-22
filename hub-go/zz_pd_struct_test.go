package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZPdStruct(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/2112810989817847808", h)
	var v map[string]any
	json.Unmarshal(gunzip(d), &v)
	data, _ := v["data"].(map[string]any)
	if data != nil {
		keys := []string{"session", "manifest", "sources", "tags", "extraInfo"}
		for _, k := range keys {
			if _, ok := data[k]; ok {
				b, _ := json.Marshal(data[k])
				s := string(b)
				if len(s) > 200 {
					s = s[:200]
				}
				fmt.Printf("%s = %s\n", k, s)
			}
		}
	}
}
