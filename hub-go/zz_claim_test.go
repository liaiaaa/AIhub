package main

// 通用领取器：对指定 task_code 列表，若 progress 达标且 accepted/in_progress，
// 调 POST /activity/growth/tasks/{code}/claim 领取积分。
// 用法：go test -run TestZZClaimTasks -v . （TASK_CODES 环境变量逗号分隔）

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestZZClaimTasks(t *testing.T) {
	codes := os.Getenv("TASK_CODES")
	if codes == "" {
		t.Skip("设 TASK_CODES 环境变量")
	}
	want := map[string]bool{}
	for _, c := range strings.Split(codes, ",") {
		want[strings.TrimSpace(c)] = true
	}
	cred := os.Getenv("CRED")
	if cred == "" {
		cred = "国内workbuddy凭证"
	}
	dir := domCredDirs()[cred]
	if dir == "" {
		t.Fatalf("未知凭证: %s", cred)
	}
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()

	// 拉任务状态
	b, _, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", headers)
	var env struct {
		Data struct {
			Tasks []struct {
				TaskCode     string `json:"task_code"`
				AcceptStatus string `json:"accept_status"`
				Progress     *struct {
					Current int `json:"current"`
					Target  int `json:"target"`
				} `json:"progress"`
				RewardCredit int `json:"reward_credit"`
			} `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(b, &env)
	for _, tk := range env.Data.Tasks {
		if !want[tk.TaskCode] {
			continue
		}
		if tk.AcceptStatus == "claimed" {
			fmt.Printf("  %s: 已领取，跳过\n", tk.TaskCode)
			continue
		}
		if tk.Progress == nil || tk.Progress.Current < tk.Progress.Target {
			cur, tgt := 0, 0
			if tk.Progress != nil {
				cur, tgt = tk.Progress.Current, tk.Progress.Target
			}
			fmt.Printf("  %s: 未达标 %d/%d，跳过\n", tk.TaskCode, cur, tgt)
			continue
		}
		// 达标，领取
		url := fmt.Sprintf("%s/activity/growth/tasks/%s/claim", p.Backend, tk.TaskCode)
		resp, status, err := httpPostJSON(url, headers, []byte("{}"))
		fmt.Printf("  CLAIM %s (%d/%d, +%d分) HTTP %d: %s (err=%v)\n",
			tk.TaskCode, tk.Progress.Current, tk.Progress.Target, tk.RewardCredit, status, truncate(string(resp), 250), err)
	}
}
