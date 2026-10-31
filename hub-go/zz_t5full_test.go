package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZT5Full(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", headers)
	var env struct {
		Data struct {
			Tasks []map[string]any `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(b, &env)
	for _, tk := range env.Data.Tasks {
		c, _ := tk["task_code"].(string)
		if c == "template_5" || c == "expert_5" || c == "automation_1" || c == "Library_read" {
			pp, _ := json.Marshal(tk)
			fmt.Printf("%s\n", string(pp))
		}
	}
}
