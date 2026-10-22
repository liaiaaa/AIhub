package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestZZExpPost(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	body := `{"prompt":"你好，用一句话自我介绍","model":"hy3","conversationOrigin":"agents","expertId":"ex_aqOoHk4LRmvK"}`
	b, s, _ := httpPostJSON(p.Backend+"/console/as/conversations/", h, []byte(body))
	ds := string(gunzip(b))
	fmt.Printf("POST HTTP %d: %s\n", s, truncate(ds, 150))
	var r struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(ds), &r)
	if r.Data.ID == "" {
		return
	}
	time.Sleep(6 * time.Second)
	d, _, _ := httpGetJSON(p.Backend+"/console/as/conversations/"+r.Data.ID, h)
	dd := string(gunzip(d))
	for _, seg := range []string{"X_EXPERT_ID", "expertId", "ex_aqOoHk4LRmvK"} {
		i := strings.Index(dd, seg)
		fmt.Printf("  %s: %v\n", seg, i >= 0)
	}
}
