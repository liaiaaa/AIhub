package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestZZCbIds2(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	reScene := regexp.MustCompile(`"sceneId":"?(\d+)"?|scene:(\d+)`)
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
			if !strings.Contains(c.Name, "场景") && !strings.Contains(c.Name, "拼豆") && !strings.Contains(c.Name, "高考") {
				continue
			}
			d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+c.ID, h)
			ds := string(gunzip(d))
			var hits []string
			for _, m := range reScene.FindAllStringSubmatch(ds, -1) {
				v := m[1]
				if v == "" {
					v = m[2]
				}
				hits = append(hits, v)
			}
			exp := regexp.MustCompile(`ex_[A-Za-z0-9]+`).FindAllString(ds, -1)
			fmt.Printf("%-22s scene=%v exp=%v len=%d\n", strings.ReplaceAll(c.Name[:min(18,len([]rune(c.Name)))], " ", ""), uniq(hits), uniq(exp), len(ds))
			time.Sleep(150 * time.Millisecond)
		}
	}
}
