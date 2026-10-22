package main

import (
	"fmt"
	"testing"
)

func TestZZCbRisk(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, err := cm.Headers()
	fmt.Println("Headers err:", err)
	// 任务列表（风控时通常这里返回非0或空）
	b, s, _ := httpGetJSON(p.Backend+"/v2/activity/growth/tasks", h)
	fmt.Printf("tasks HTTP %d: %s\n", s, truncate(string(b), 200))
	// profile
	b2, s2, _ := httpGetJSON(p.Backend+"/v2/user/profile", h)
	fmt.Printf("profile HTTP %d: %s\n", s2, truncate(string(b2), 200))
}
