package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZDomProg(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", h)
	var r struct {
		Data struct {
			Tasks []struct {
				TaskCode     string `json:"task_code"`
				AcceptStatus string `json:"accept_status"`
				Progress     *struct {
					Current int `json:"current"`
					Target  int `json:"target"`
				} `json:"progress"`
			} `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(gunzip(b), &r)
	for _, tk := range r.Data.Tasks {
		if tk.AcceptStatus == "claimed" {
			continue
		}
		c, tg := 0, 0
		if tk.Progress != nil {
			c, tg = tk.Progress.Current, tk.Progress.Target
		}
		fmt.Printf("%-22s %d/%d (%s)\n", tk.TaskCode, c, tg, tk.AcceptStatus)
	}
}
