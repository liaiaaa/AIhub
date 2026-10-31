package main

// 网页版对话后：查 first_chat 徽章 + chat_5 完整 JSON，判断网页对话是否计入

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZBadgeCheck(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()

	// first_chat 徽章
	b, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/badges", headers)
	var bd struct {
		Data struct {
			Badges []map[string]any `json:"badges"`
		} `json:"data"`
	}
	json.Unmarshal(b, &bd)
	fmt.Printf("=== 徽章（共 %d）===\n", len(bd.Data.Badges))
	for _, x := range bd.Data.Badges {
		code, _ := x["code"].(string)
		if code == "first_chat" || code == "skill_installed" {
			pp, _ := json.Marshal(x)
			fmt.Printf("  %s\n", string(pp))
		}
	}

	// chat_5 完整字段
	b2, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", headers)
	var env struct {
		Data struct {
			Tasks []map[string]any `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(b2, &env)
	fmt.Println("=== chat_5 / first_chat 任务完整字段 ===")
	for _, tk := range env.Data.Tasks {
		code, _ := tk["task_code"].(string)
		if code == "chat_5" || code == "first_buddy" {
			pp, _ := json.Marshal(tk)
			fmt.Printf("  %s\n", truncate(string(pp), 900))
		}
	}
}
