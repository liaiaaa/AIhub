package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"testing"
	"time"
)

func gunzip(b []byte) []byte {
	if zr, e := gzip.NewReader(bytes.NewReader(b)); e == nil {
		out, _ := io.ReadAll(zr)
		return out
	}
	return b
}

func TestZZCbIds(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	reScene := regexp.MustCompile(`scene:\d+|"sceneId":\s*"?(\d+)`)
	reExp := regexp.MustCompile(`ex_[A-Za-z0-9]+`)
	seen := map[string]bool{}
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
			key := c.Name
			if len(key) > 18 {
				key = key[:18]
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+c.ID, h)
			ds := string(gunzip(d))
			var tags []string
			for _, m := range reScene.FindAllStringSubmatch(ds, -1) {
				if m[1] != "" {
					tags = append(tags, "scene:"+m[1])
				} else {
					tags = append(tags, m[0])
				}
			}
			var exps []string
			for _, m := range reExp.FindAllString(ds, -1) {
				exps = append(exps, m)
			}
			if len(tags) > 0 || len(exps) > 0 {
				fmt.Printf("%-20s scenes=%v exps=%v\n", key, uniq(tags), uniq(exps))
			}
			time.Sleep(120 * time.Millisecond)
		}
	}
}

func uniq(in []string) []string {
	m := map[string]bool{}
	var o []string
	for _, s := range in {
		if !m[s] {
			m[s] = true
			o = append(o, s)
		}
	}
	return o
}
