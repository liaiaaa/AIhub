package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestZZExpBind(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	labels := []string{"expertId", "expertCode", "agentName"}
	vals := []string{"SeniorDeveloper", "SeniorDeveloper", "senior-developer"}
	for i := range labels {
		body := fmt.Sprintf(`{"prompt":"你好，一句话自我介绍","model":"hy3","conversationOrigin":"agents","%s":"%s"}`, labels[i], vals[i])
		b, s, _ := httpPostJSON(p.Backend+"/console/as/conversations/", h, []byte(body))
		var r struct {
			Code int    `json:"code"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(gunzip(b), &r)
		fmt.Printf("%s => code=%d HTTP %d id=%s\n", labels[i], r.Code, s, r.Data.ID)
		time.Sleep(500 * time.Millisecond)
	}
}
