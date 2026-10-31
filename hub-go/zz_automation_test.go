package main

// automation_1：通过 scheduler API 建一个定时任务（网页版同一接口）。

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZAutomation(t *testing.T) {
	requireLive(t)
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()
	headers["X-Product-Code"] = "workbuddy"
	headers["Origin"] = "https://www.workbuddy.cn"
	headers["Referer"] = "https://www.workbuddy.cn/app"

	body := map[string]any{
		"name":          "每周五自动生成周报",
		"agentType":     "personal_agent",
		"ownerType":     "personal",
		"frequencyType": "WEEKLY",
		"cronExpr":      "0 0 9 * * 5",
		"timezone":      "Asia/Shanghai",
		"agentConfig": map[string]any{
			"prompt":     "帮我生成本周工作周报，包含本周完成事项、下周计划、风险点。",
			"model":      "hy3",
			"connectors": []any{},
		},
	}
	bb, _ := json.Marshal(body)
	resp, status, err := httpPostJSON(p.Backend+"/v2/as/scheduler/tasks", headers, bb)
	fmt.Printf("POST create HTTP %d: %s (err=%v)\n", status, truncate(string(resp), 400), err)

	// 复查列表
	l, _, _ := httpGetJSON(p.Backend+"/v2/as/scheduler/tasks", headers)
	fmt.Printf("列表: %s\n", truncate(string(l), 500))
}
