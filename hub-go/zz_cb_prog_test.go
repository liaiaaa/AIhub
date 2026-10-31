package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

func TestZZCbProg(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, s, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", h)
	if zr, e := gzip.NewReader(bytes.NewReader(b)); e == nil {
		b, _ = io.ReadAll(zr)
	}
	var r struct {
		Data struct {
			Tasks []struct {
				TaskCode string `json:"task_code"`
				Progress struct {
					Current int `json:"current"`
					Target  int `json:"target"`
				} `json:"progress"`
				AcceptStatus string `json:"accept_status"`
			} `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(b, &r)
	fmt.Printf("HTTP %d — CodeBuddy号任务进度：\n", s)
	for _, tk := range r.Data.Tasks {
		if tk.AcceptStatus != "claimed" {
			fmt.Printf("  %-24s %d/%d (%s)\n", tk.TaskCode, tk.Progress.Current, tk.Progress.Target, tk.AcceptStatus)
		}
	}
}
