package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
)

func TestZZCbConvs(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, s, _ := httpGetJSON(p.Backend+"/console/as/conversations/?limit=20", h)
	var r struct {
		Data struct {
			Conversations []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Status  string `json:"status"`
				Origin  string `json:"conversationOrigin"`
				Extra   any    `json:"extraInfo"`
			} `json:"conversations"`
		} `json:"data"`
	}
	json.Unmarshal(gunzip(b), &r)
	fmt.Printf("HTTP %d 共%d条:\n", s, len(r.Data.Conversations))
	reD := regexp.MustCompile(`\d{8,12}`)
	for _, c := range r.Data.Conversations {
		es := fmt.Sprint(c.Extra)
		tags := reD.FindAllString(es, 3)
		fmt.Printf("  %-16s %-9s %s tags=%v\n", c.Name[:min(16, len([]rune(c.Name)))], c.Status, c.Origin, tags)
	}
}
