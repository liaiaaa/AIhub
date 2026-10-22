package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestZZChk(t *testing.T) {
	dir := domCredDirs()["国内workbuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	headers, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/activity/growth/buddy/travel/status", headers)
	var env struct {
		Data map[string]any `json:"data"`
	}
	json.Unmarshal(b, &env)
	fmt.Printf("travel: %v\n", env.Data)
	if aa, ok := env.Data["arrive_at"].(float64); ok {
		fmt.Println("到达时间:", time.Unix(int64(aa), 0).Format("15:04:05"))
	}
	l, _, _ := httpGetJSON(p.Backend+"/v2/as/scheduler/tasks", headers)
	fmt.Printf("scheduler: %s\n", truncate(string(l), 400))
}
