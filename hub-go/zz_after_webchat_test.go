package main

// 网页版首聊后核对：uid 对齐 + chat_5 进度（两个凭证都查）

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZAfterWebChat(t *testing.T) {
	requireLive(t)
	for _, credName := range []string{"国内workbuddy凭证", "CodeBuddy凭证"} {
		dir := domCredDirs()[credName]
		p := NewDomProxyDir(dir)
		cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
		headers, err := cm.Headers()
		if err != nil {
			fmt.Printf("=== %s 取头失败 %v\n", credName, err)
			continue
		}
		fmt.Printf("=== %s ===\n", credName)
		for k := range headers {
			if k == "X-User-Id" {
				fmt.Printf("  uid=%s\n", headers[k])
			}
		}
		b, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", headers)
		var env struct {
			Data struct {
				Tasks []struct {
					TaskCode string `json:"task_code"`
					Progress *struct {
						Current int `json:"current"`
						Target  int `json:"target"`
					} `json:"progress"`
				} `json:"tasks"`
			} `json:"data"`
		}
		json.Unmarshal(b, &env)
		for _, tk := range env.Data.Tasks {
			if tk.TaskCode == "chat_5" && tk.Progress != nil {
				fmt.Printf("  chat_5: %d/%d\n", tk.Progress.Current, tk.Progress.Target)
			}
		}
	}
}
