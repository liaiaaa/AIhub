package main

// 拉剩余任务的精确描述与跳转，规划自动化

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZTaskDesc(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", headers)
	var env struct {
		Data struct {
			Tasks []struct {
				TaskCode     string `json:"task_code"`
				Title        string `json:"title"`
				Desc         string `json:"description"`
				Jump         string `json:"jump_url"`
				AcceptStatus string `json:"accept_status"`
				Progress     *struct {
					Current int `json:"current"`
					Target  int `json:"target"`
				} `json:"progress"`
			} `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(b, &env)
	for _, tk := range env.Data.Tasks {
		if tk.AcceptStatus == "claimed" {
			continue
		}
		c, tg := 0, 0
		if tk.Progress != nil {
			c, tg = tk.Progress.Current, tk.Progress.Target
		}
		fmt.Printf("%-22s %d/%d | %s | %s | %s\n", tk.TaskCode, c, tg, tk.Title, tk.Desc, tk.Jump)
	}
}
