package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
)

func TestZZSceneKeys(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/v2/as/support/scenes", h)
	ds := string(gunzip(b))
	re := regexp.MustCompile(`\d{9,13}`)
	fmt.Println("大数字:", uniq(re.FindAllString(ds, 20)))
	var r struct {
		Data struct {
			Scenes []map[string]any `json:"scenes"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(ds), &r)
	if len(r.Data.Scenes) > 0 {
		for k, v := range r.Data.Scenes[0] {
			s := fmt.Sprint(v)
			if len(s) > 90 {
				s = s[:90]
			}
			fmt.Printf("  %-14s = %s\n", k, s)
		}
	}
}
